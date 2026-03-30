# `internal/config`

This package is the **single source of truth** for initializing, reading, and updating configuration across all Datadog products (tracer, profiler, etc.).

## Architecture

Configuration is split into two layers:

- **`BaseConfig`** — the single source of truth for all configuration. Loaded once from config sources (env, declarative, defaults, RC) and shared via pointer. A field lives here regardless of which product reads it, unless it requires a shadow override (see below).
- **Product configs** (`TracerConfig`, `ProfilerConfig`, ...) — embed `*BaseConfig` so shared getters are promoted automatically. Product configs exist **only** to hold shadow fields.

### Shadow fields

**Rule: if a field has a programmatic API (`With*` option) on any product, it gets a shadow field on that product's config.**

A shadow field is a local override pointer (`*bool`, `*string`, etc.) on the product config; `nil` means "use the `BaseConfig` value." The getter checks the local override first and falls through to `BaseConfig` if unset.

This rule is intentionally simple and forward-looking. Even if only one product has a `With*` option for a field today, making it a shadow from the start means that when a second product adds a `With*` for the same field, the first product doesn't need refactoring — each product already has its own independent override.

Environment variables and Remote Config updates flow through `BaseConfig` and affect all products automatically, while a product-level programmatic override only affects the calling product.

### Constructors

| Function | Returns | Use |
|---|---|---|
| `GetBaseConfig()` | `*BaseConfig` | Shared singleton. Default choice for most packages. |
| `GetTracerConfig()` | `*TracerConfig` | Tracer-owned config with product overrides. Called once at tracer startup. |
| `GetProfilerConfig()` | `*ProfilerConfig` | Profiler-owned config with product overrides. Called once at profiler startup. |

All constructors share a single config provider so declarative config (YAML) is parsed only once.

### File layout

| File | Contents |
|---|---|
| `config.go` | `BaseConfig` type, singleton/constructor logic, shared getters & setters |
| `tracerconfig.go` | `TracerConfig` type, shadow field overrides |
| `profilerconfig.go` | `ProfilerConfig` type, shadow field overrides |
| `config_helpers.go` | Shared helpers (URL resolution, validation, etc.) |

## Migration guidelines

When migrating a configuration value from another package (e.g. `ddtrace/tracer`):

- **Decide scope**: most fields belong on `BaseConfig`. If the field has a programmatic API (`With*` option), add a shadow field on the corresponding product config (see "Shadow fields" above).
- **Initialize it in the load function**: `loadBaseConfig()` reads from the config provider. Product load functions (`loadTracerConfig`, `loadProfilerConfig`) only set the `BaseConfig` pointer; they don't read from the provider.
- **Expose an accessor**: add a getter (and a setter if the value is updated at runtime).
- **Report telemetry in setters**: setters should call `configtelemetry.Report(...)` with the correct origin.
- **Update callers**: replace reads/writes on local "config" structs with calls to the product config (e.g. `GetTracerConfig()`).
- **Delete old state**: remove the migrated field from any legacy config structs once no longer referenced.
- **Update tests**: tests should call the setter/getter (or set env vars) rather than mutating legacy fields.

Sample migration PR: https://github.com/DataDog/dd-trace-go/pull/4214

## Hot paths & performance guidelines

Some configuration accessors may be called in hot paths (e.g., span start/finish, partial flush logic).
If benchmarks regress, ensure getters are efficient and do not:

- **Copy whole maps/slices on every call**: prefer single-key lookup helpers like `ServiceMapping`/`HasFeature` over returning a map copy.
- **Take multiple lock/unlock pairs to read related fields**: prefer a combined getter under one `RLock`, like `PartialFlushEnabled()`.
- **Rethink `defer` in per-span/tight-loop getters**: avoid `defer` in getters that are executed extremely frequently.

### Cache config reads before loops (especially retry loops)

If you're reading a config value inside **any** loop, prefer caching it once into a **local variable** before the loop:

- **Why**: avoids repeated `RLock/RUnlock` overhead per iteration and keeps loop bounds/logging consistent if the value ever becomes dynamically updatable.
- **Example**: cache `SendRetries()` and `RetryInterval()` once per flush send, and use the cached values inside the loop.

```go
sendRetries := cfg.SendRetries()
retryInterval := cfg.RetryInterval()
for attempt := 0; attempt <= sendRetries; attempt++ {
	// ...
	time.Sleep(retryInterval)
}
```
