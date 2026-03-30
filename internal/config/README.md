# `internal/config`

This package is the **single source of truth** for initializing, reading, and updating configuration across all Datadog products (tracer, profiler, etc.).

## Architecture

Configuration is split into two layers:

- **`GlobalConfig`** holds fields shared across all products (service name, env, version, agent URL, etc.). A single instance is created lazily and shared via pointer.
- **Product configs** (`TracerConfig`, `ProfilerConfig`, ...) embed `*GlobalConfig` so global getters are promoted automatically. Each product config adds its own product-specific fields.

Product configs can **shadow** global fields (e.g. `serviceName`) with local override pointers. When a programmatic API like `WithService` is called, it sets the product-local override. Getters check the local override first; if nil, they fall through to `GlobalConfig`. This means environment variables and Remote Config updates affect all products, while programmatic APIs only affect the calling product.

### Constructors

| Function | Returns | Use |
|---|---|---|
| `GetTracerConfig()` | `*TracerConfig` | New tracer-owned config pointing at the shared `GlobalConfig` |
| `GetProfilerConfig()` | `*ProfilerConfig` | New profiler-owned config pointing at the shared `GlobalConfig` |
| `Get()` | `*Config` (= `*TracerConfig`) | Legacy singleton, prefer `GetTracerConfig()` |
| `CreateNew()` | `*Config` | Legacy, prefer `GetTracerConfig()` |

All constructors share a single config provider so declarative config (YAML) is parsed only once.

### File layout

| File | Contents |
|---|---|
| `config.go` | `GlobalConfig` type, singleton/constructor logic, global getters & setters |
| `tracerconfig.go` | `TracerConfig` type, shadow overrides, tracer-specific getters & setters |
| `profilerconfig.go` | `ProfilerConfig` type, profiler-specific getters & setters |
| `config_helpers.go` | Shared helpers (URL resolution, validation, etc.) |

## Migration guidelines

When migrating a configuration value from another package (e.g. `ddtrace/tracer`):

- **Decide scope**: if the field is shared across products, add it to `GlobalConfig`. If it is product-specific, add it to the appropriate product config (e.g. `TracerConfig`).
- **Initialize it in the load function**: `loadGlobalConfig()` for global fields, `loadTracerConfig()` / `loadProfilerConfig()` for product fields. Read from the config provider, which iterates over the following sources, in order, returning the default if no valid value found: local declarative config file, OTEL env vars, env vars, managed declarative config file.
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
