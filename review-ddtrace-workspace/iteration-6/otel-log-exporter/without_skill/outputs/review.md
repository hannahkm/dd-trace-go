# Code Review: PR #4350 — feat(otel): adding support for OpenTelemetry logs

**PR Author:** rachelyangdog
**Status at review time:** MERGED
**Reviewer:** Senior Go engineer (AI review)

---

## Summary

This PR adds a new package `ddtrace/opentelemetry/log` that wires up an OpenTelemetry Logs SDK pipeline inside the Datadog Go tracer. When `DD_LOGS_OTEL_ENABLED=true`, the tracer initializes a BatchLogRecordProcessor backed by an OTLP exporter (HTTP/JSON, HTTP/protobuf, or gRPC). It also introduces a `ddAwareLogger` wrapper that bridges Datadog span context into the OTel `context.Context` so that log records are correlated with the right trace/span IDs.

The overall structure is reasonable and the test coverage is good. However there are several correctness issues, design concerns, and Go-idiom issues worth flagging.

---

## Issues

### 1. `sync.Once` is reassigned to reset state — this is unsafe (Bug / Correctness)

**File:** `ddtrace/opentelemetry/log/logger_provider.go`

```go
// Reset the singleton state so it can be reinitialized
globalLoggerProvider = nil
globalLoggerProviderWrapper = nil
globalLoggerProviderOnce = sync.Once{}
```

Reassigning a `sync.Once` by value while under a mutex is _not_ safe because any goroutine that already captured a reference to the old `Once` (there isn't one here explicitly, but it's still a misuse) could observe torn state. More importantly, this pattern is a footgun because the `sync` package documentation explicitly warns against copying `sync.Once` after first use, and replacing it wholesale under a lock just to allow reinitialization is an architectural smell.

**Recommended fix:** Replace the `sync.Once` with a boolean `initialized` field protected by the existing `sync.Mutex`. Alternatively, use a two-step pattern: check the boolean under a read lock, take the write lock, check again, then initialize. This also avoids holding the write lock during expensive network initialization in `InitGlobalLoggerProvider`.

---

### 2. `InitGlobalLoggerProvider` holds the lock during expensive I/O (Performance / Deadlock Risk)

**File:** `ddtrace/opentelemetry/log/logger_provider.go`

```go
func InitGlobalLoggerProvider(ctx context.Context) error {
    var err error
    globalLoggerProviderOnce.Do(func() {
        globalLoggerProviderMu.Lock()
        defer globalLoggerProviderMu.Unlock()
        ...
        exporter, exporterErr := newOTLPExporter(ctx, nil, nil)
        ...
    })
    return err
}
```

`newOTLPExporter` creates a gRPC or HTTP client and may make network calls (e.g., the gRPC exporter dials the server). Holding the mutex during that entire duration blocks `GetGlobalLoggerProvider` (which only reads) and any concurrent `Stop` call, which needs the same mutex.

Since `sync.Once` already serializes initialization, the lock inside `Once.Do` is redundant for the initialization path. The lock is only needed when resetting (in `ShutdownGlobalLoggerProvider`). The pattern should be: use `Once` for initialization without the lock, then use the lock only in the reset path.

---

### 3. `IsRecording` always returns `true` even for finished spans (Correctness)

**File:** `ddtrace/opentelemetry/log/correlation.go`

```go
func (w *ddSpanWrapper) IsRecording() bool {
    // This always returns true because DD spans don't expose a "finished" state
    return true
}
```

The comment acknowledges this limitation but accepts it too readily. The OTel spec states that `IsRecording` returning `true` means "the span is actively collecting data." If a span has already been finished (via `Finish()`), returning `true` is semantically incorrect and could mislead OTel instrumentation that uses `IsRecording` to gate expensive operations.

While the Datadog span API does not expose `IsFinished()` publicly, the fact that this always returns `true` means any OTel log bridge that checks `IsRecording()` before emitting will behave incorrectly if it encounters a finished DD span in the context (e.g., held in a goroutine that outlives the span's lifetime).

At minimum this should be documented as a known limitation in the package-level docs, and a TODO should be filed to add `IsFinished()` to the tracer public API.

---

### 4. Hostname precedence logic is inverted from what the comments say (Correctness / Documentation)

**File:** `ddtrace/opentelemetry/log/resource.go`

```go
// Step 4: Handle hostname with special rules
// OTEL_RESOURCE_ATTRIBUTES[host.name] has highest priority - never override it
if _, hasOtelHostname := otelAttrs["host.name"]; !hasOtelHostname {
    hostname, shouldAddHostname := resolveHostname()
    if shouldAddHostname && hostname != "" {
        attrs["host.name"] = hostname
    }
}
```

But earlier in step 3, DD_TAGS is applied over the `attrs` map (which already contains `otelAttrs`), so any `host.name` tag in `DD_TAGS` _would_ overwrite an OTel `host.name` set via `OTEL_RESOURCE_ATTRIBUTES`. This contradicts the stated invariant in the comment at the top of `buildResource`:

> "OTEL_RESOURCE_ATTRIBUTES[host.name] always wins"

The test `TestComplexScenarios/"DD overrides OTEL for service/env/version except hostname"` passes because it doesn't set `host.name` in `DD_TAGS` — but if a user has `DD_TAGS=host.name:custom-host` alongside `OTEL_RESOURCE_ATTRIBUTES=host.name=otel-host`, the DD tag wins, contrary to the documented behavior.

**Fix:** After applying `DD_TAGS`, restore any `host.name` from `otelAttrs` if it was present, or explicitly filter `host.name` out when iterating `ddTags`.

---

### 5. `sanitizeOTLPEndpoint` appends the signal path unconditionally in a way that can mangle already-correct paths (Bug)

**File:** `ddtrace/opentelemetry/log/exporter.go`

```go
} else if !strings.HasSuffix(u.Path, signalPath) {
    // If path doesn't already end with signal path, append it
    u.Path = u.Path + signalPath
}
```

If a user sets `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=http://host:4320/custom-prefix`, this code appends `/v1/logs` and produces `http://host:4320/custom-prefix/v1/logs`. This is wrong — the OTel spec says `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` is a _full_ endpoint URL and the SDK should use it as-is (no path mangling). The HTTP OTLP exporter option `WithEndpointURL` already handles this correctly if you just pass the raw URL through. The sanitize logic should only strip trailing slashes, not add signal-specific path suffixes.

Furthermore, for the `OTEL_EXPORTER_OTLP_ENDPOINT` (base endpoint without signal path), the spec says the SDK appends `/v1/logs` itself — so there's a risk of double-appending if `WithEndpointURL` is used instead of `WithEndpoint` + `WithURLPath`.

The correct approach is:
- For `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` (signal-specific): use as-is via `WithEndpointURL`
- For `OTEL_EXPORTER_OTLP_ENDPOINT` (generic): strip trailing slash, append `/v1/logs`, use via `WithEndpointURL`

---

### 6. gRPC endpoint resolution silently ignores the `https` scheme for `DD_TRACE_AGENT_URL` (Bug)

**File:** `ddtrace/opentelemetry/log/exporter.go`

```go
insecure = (u.Scheme == "http" || u.Scheme == "unix")
```

If `DD_TRACE_AGENT_URL=https://agent:8126`, `insecure` is correctly `false`. But for gRPC, not calling `WithInsecure()` means TLS is assumed — which is correct. However, for the gRPC path, the scheme `grpc` is treated as insecure (`u.Scheme == "grpc"` — see the comment), but `grpcs` is not listed. The OTel SDK uses `grpc` and `grpcs` as scheme conventions. This inconsistency between the HTTP and gRPC endpoint parsing could silently send logs over plain-text gRPC when the user intends TLS.

---

### 7. `telemetryExporter.Export` counts records even on error (Correctness / Telemetry Accuracy)

**File:** `ddtrace/opentelemetry/log/exporter.go`

```go
func (e *telemetryExporter) Export(ctx context.Context, records []sdklog.Record) error {
    err := e.Exporter.Export(ctx, records)
    if len(records) > 0 {
        e.telemetry.RecordLogRecords(len(records))
    }
    return err
}
```

The comment says "Record the number of log records exported (success or failure)." Recording on failure inflates the counter and misrepresents actual successful exports. If the intent is to count _attempted_ exports, the metric name and docs should reflect "attempted" not "exported." If the intent is successful exports only, the check should be `if err == nil && len(records) > 0`. The PR description says the metric tracks "the number of log records exported" which implies success — the current implementation doesn't match that description.

---

### 8. Package name collision: `log` shadows standard library and internal packages (Go Idiom)

**File:** `ddtrace/opentelemetry/log/` (all files)

The package is named `log`. This collides with:
- The Go standard library `log` package
- The internal `github.com/DataDog/dd-trace-go/v2/internal/log` package used in this very package

Every file in this package imports `github.com/DataDog/dd-trace-go/v2/internal/log` as `log`, which creates a confusing and fragile import alias situation. Any consumer who imports both this package and the standard `log` or internal `log` package will have a conflict.

The package should be renamed to something unambiguous: `ddotellog`, `otlplog`, `otellogbridge`, etc. This is a public-facing API concern since `GetGlobalLoggerProvider()` is exported.

---

### 9. `ForceFlush` inconsistently ignores the provided context (API Design)

**File:** `ddtrace/opentelemetry/log/integration.go`

```go
func ForceFlush(ctx context.Context) error {
    ...
    return provider.ForceFlush(ctx)
}
```

`ForceFlush` accepts a context, which is correct. But `Stop()` does not accept a context at all and creates its own with a hardcoded 5-second timeout:

```go
func Stop() error {
    ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
    defer cancel()
    return ShutdownGlobalLoggerProvider(ctx)
}
```

This is inconsistent with the rest of the API and prevents callers from controlling shutdown timeout. If the tracer is shutting down in a context with a tighter deadline (e.g., a Lambda handler), the caller cannot propagate that deadline. `Stop` should accept a `context.Context` like every other similar function in the package.

---

### 10. `buildResource` re-implements attribute collection already done by the OTel SDK (Overengineering)

**File:** `ddtrace/opentelemetry/log/resource.go`

The function manually reads `OTEL_RESOURCE_ATTRIBUTES`, parses it into a map, overlays DD attributes, then converts back to `attribute.KeyValue` slice. The OTel SDK `resource.WithFromEnv()` detector already reads and parses `OTEL_RESOURCE_ATTRIBUTES` automatically. The correct pattern is:

```go
resource.New(ctx,
    resource.WithFromEnv(),         // reads OTEL_RESOURCE_ATTRIBUTES
    resource.WithTelemetrySDK(),
    resource.WithAttributes(ddAttrs...), // DD attrs override by being applied last
)
```

However, due to resource merging semantics (later detectors take lower priority, not higher), this ordering alone may not give DD precedence. The correct OTel way to give DD attributes precedence is to use `resource.Merge(otelResource, ddResource)` with the DD resource as the "base" (second argument wins in the current merge semantics). The current hand-rolled approach works but duplicates logic the SDK already provides and could diverge from SDK behavior on edge cases like percent-encoded values in `OTEL_RESOURCE_ATTRIBUTES`.

---

### 11. `resolveBLRPScheduleDelay` reuses `parseTimeout` which is misleadingly named (Go Idiom / Clarity)

**File:** `ddtrace/opentelemetry/log/exporter.go`

```go
func resolveBLRPScheduleDelay() time.Duration {
    if delayStr := env.Get(envBLRPScheduleDelay); delayStr != "" {
        if delay, err := parseTimeout(delayStr); err == nil {
```

`parseTimeout` is used to parse both timeout values and delay values. The name implies it's only for timeouts. Either rename it `parseMilliseconds` (which would match the same-named function in `telemetry.go` that is a duplicate), or consolidate to a single well-named helper.

Indeed, `parseMilliseconds` is defined identically in `telemetry.go`:

```go
func parseMilliseconds(value string) (int, error) {
    value = strings.TrimSpace(value)
    if ms, err := strconv.Atoi(value); err == nil {
        return ms, nil
    }
    return 0, strconv.ErrSyntax
}
```

And `parseTimeout` in `exporter.go` does essentially the same thing:
```go
func parseTimeout(str string) (time.Duration, error) {
    ms, err := strconv.ParseInt(str, 10, 64)
    ...
}
```

This is duplicate logic that should be a single shared function.

---

### 12. `ddAwareLoggerProvider` holds `*sdklog.LoggerProvider` but the interface should be against `sdklog.LoggerProvider` (API Inflexibility)

**File:** `ddtrace/opentelemetry/log/logger_provider.go`

```go
type ddAwareLoggerProvider struct {
    embedded.LoggerProvider
    underlying *sdklog.LoggerProvider
}
```

`ddAwareLoggerProvider.underlying` is typed as a concrete `*sdklog.LoggerProvider`. This means `ddAwareLoggerProvider` cannot be used in tests with a mock logger provider, and the entire design is not testable in isolation. It should accept `otellog.LoggerProvider` (the interface). The tests work around this by using `sdklog.NewLoggerProvider` directly and passing it in — but this would be cleaner if the wrapper accepted the interface.

---

### 13. Missing test isolation: tests share global state without proper cleanup (Test Correctness)

**File:** `ddtrace/opentelemetry/log/integration_test.go`, `logger_provider_test.go`

Multiple tests call `ShutdownGlobalLoggerProvider` as a cleanup step at the start, but if a test panics between initialization and cleanup, the global state leaks into the next test. Tests that rely on `config.SetUseFreshConfig(true)` also leave a deferred `config.SetUseFreshConfig(false)` which only runs on `defer`, not if the test goroutine panics.

The canonical pattern is `t.Cleanup(func() { ... })` instead of `defer` + manual cleanup at test start, which ensures cleanup runs regardless of how the test exits and is scoped to the `*testing.T` lifetime.

---

### 14. Minor: `var traceID oteltrace.TraceID; traceID = ...` double declaration (Go Idiom)

**File:** `ddtrace/opentelemetry/log/correlation.go`

```go
var traceID oteltrace.TraceID
traceID = ddCtx.TraceIDBytes()
```

This is equivalent to `traceID := ddCtx.TraceIDBytes()`. The two-step declaration without initialization adds noise. Same pattern for `spanID`.

---

## Summary Table

| # | Severity | Category | File |
|---|----------|----------|------|
| 1 | High | Bug / Correctness | `logger_provider.go` — unsafe `sync.Once` reassignment |
| 2 | Medium | Performance | `logger_provider.go` — lock held during I/O |
| 3 | Medium | Correctness | `correlation.go` — `IsRecording` always true |
| 4 | Medium | Bug | `resource.go` — hostname precedence bypass via DD_TAGS |
| 5 | Medium | Bug | `exporter.go` — `sanitizeOTLPEndpoint` path mangling |
| 6 | Low | Bug | `exporter.go` — gRPC scheme handling for `grpcs` |
| 7 | Medium | Correctness | `exporter.go` — telemetry counts failures as exports |
| 8 | High | API Design | Package named `log` conflicts with stdlib and internal packages |
| 9 | Low | API Design | `Stop()` doesn't accept `context.Context` |
| 10 | Low | Overengineering | `resource.go` re-implements OTel SDK resource detection |
| 11 | Low | Clarity | Duplicate `parseMilliseconds`/`parseTimeout` helpers |
| 12 | Low | Testability | `ddAwareLoggerProvider` holds concrete type instead of interface |
| 13 | Medium | Test Correctness | Global state leaks between tests |
| 14 | Trivial | Idiom | Redundant two-step var declarations |

---

## Overall Assessment

The PR delivers a functional feature with reasonable test coverage. The two highest-priority issues are:

1. **The package name `log`** — this is a public API problem. Importing both this package and the internal `log` package in user code will require aliasing and is confusing.
2. **The `sync.Once` reassignment** — while it works in practice under the current access pattern, it's fragile and not idiomatic Go.

The endpoint URL sanitization logic also deserves another look since it can mangle user-provided endpoint URLs in ways the OTel spec does not intend.
