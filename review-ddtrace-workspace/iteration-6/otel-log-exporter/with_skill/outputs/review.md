# Code Review: PR #4350 — feat(otel): adding support for OpenTelemetry logs

**PR:** https://github.com/DataDog/dd-trace-go/pull/4350
**Author:** rachelyangdog
**Status:** Merged (2026-02-10)
**Reviewers:** kakkoyun (approved), genesor (approved)

---

## Summary

This PR adds a new `ddtrace/opentelemetry/log/` package that implements an OpenTelemetry Logs SDK pipeline for exporting logs to the Datadog Agent via OTLP. It is opt-in via `DD_LOGS_OTEL_ENABLED=true` and supports HTTP/JSON, HTTP/protobuf, and gRPC transport protocols.

The implementation is a new standalone package (14 new Go files, ~3400 lines added). It does not hook into the tracer startup automatically — users must call `log.Start(ctx)` manually, following the same model as OTel metrics.

---

## Overall Assessment

The code is well-structured and thoroughly commented. The architecture is clean, test coverage is reasonable (76% patch coverage), and the implementation follows established patterns in the codebase. Two reviewers approved, and the PR was merged. My review below focuses on issues that were either not caught or not fully addressed in the original review cycle.

---

## Issues Found

### Critical / Correctness

**1. Telemetry count is recorded even on export failure**

In `exporter.go`, `telemetryExporter.Export` records the log record count unconditionally regardless of whether the underlying export succeeded:

```go
func (e *telemetryExporter) Export(ctx context.Context, records []sdklog.Record) error {
    err := e.Exporter.Export(ctx, records)
    // Record the number of log records exported (success or failure)
    if len(records) > 0 {
        e.telemetry.RecordLogRecords(len(records))
    }
    return err
}
```

The comment says "success or failure" as if this is intentional, but the metric is named `otel.log_records` (implying records exported), not `otel.log_export_attempts`. If the metric is meant to track successful exports, failed exports should not be counted, or a separate error counter should be added. This is a semantic bug if the metric is used to measure throughput at the receiver.

**Recommendation:** Track success and failure separately, or rename the metric to `otel.log_export_attempts` to make the semantics explicit.

---

**2. `sanitizeOTLPEndpoint` incorrectly appends the signal path to any non-empty path**

In `exporter.go`:

```go
func sanitizeOTLPEndpoint(rawURL, signalPath string) string {
    // ...
    if u.Path == "" {
        u.Path = signalPath
    } else if !strings.HasSuffix(u.Path, signalPath) {
        // If path doesn't already end with signal path, append it
        u.Path = u.Path + signalPath
    }
    return u.String()
}
```

If a user sets `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=http://collector:4318/custom/prefix`, this function will produce `http://collector:4318/custom/prefix/v1/logs`. The OTel specification says `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` is a full URL and the SDK must use it as-is, not append a path to it. The `otlploghttp.WithEndpointURL(url)` API already handles the full URL — there is no need to sanitize or append paths.

This behavior diverges from the OTel specification and could break users who set a custom endpoint that does not end in `/v1/logs`.

**Recommendation:** When `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` is set, pass the URL directly to `otlploghttp.WithEndpointURL` after only stripping trailing slashes. Do not append the signal path.

---

### Design / Architecture

**3. Direct env var reading instead of `internal/config`**

Reviewer `genesor` flagged this, and the response was that `internal/config` was only used for `DD_LOGS_OTEL_ENABLED`. All OTLP-specific env vars (`OTEL_EXPORTER_OTLP_*`, `OTEL_BLRP_*`) are read directly via `env.Get`. This means:

- No support for config sources other than environment variables
- No automatic telemetry reporting for these configs via the config system (telemetry is manually wired in `telemetry.go` instead)
- Inconsistent with how other env vars are handled in the tracer

This was a conscious decision documented in the PR discussion, but it leaves technical debt. The manual telemetry wiring in `telemetry.go` is verbose (~200 lines) and partially duplicates functionality already in `internal/config`.

---

**4. No tracer lifecycle integration**

The `Start()` and `Stop()` functions in `integration.go` are public but not called from the tracer's `Start`/`Stop`. The PR description states this is intentional (matching OTel metrics behavior), but there are practical problems:

- Users who forget to call `Stop()` will leak goroutines from the batch processor
- No documentation or example in the package shows how to call `Start()`/`Stop()` correctly
- The PR checklist item for system tests is unchecked

Reviewer `genesor` asked for an `example_test.go` and the author responded that docs would be added externally, but nothing was added to the package itself.

**Recommendation:** Add an `example_test.go` showing the basic lifecycle (start tracer, call `log.Start`, emit logs, call `log.Stop`).

---

**5. `ddSpanWrapper.IsRecording()` always returns `true`**

In `correlation.go`:

```go
func (w *ddSpanWrapper) IsRecording() bool {
    // This always returns true because DD spans don't expose a "finished" state
    // through the public API.
    return true
}
```

The comment acknowledges the limitation. However, if a log is emitted after `span.Finish()` with a context that still holds the finished span, the log will be incorrectly associated with a finished (and likely already exported) span. This could lead to logs with trace/span IDs that have no corresponding spans in the backend, causing confusing UX.

This is a known limitation of the DD span API, but it should be documented clearly, and ideally a future improvement should track span completion state.

---

**6. Hostname precedence is inverted from stated intent**

The docstring for `buildResource` states:

> Datadog hostname takes precedence over OTEL hostname if both are present

But the implementation does the opposite:

```go
// OTEL_RESOURCE_ATTRIBUTES[host.name] has highest priority - never override it
if _, hasOtelHostname := otelAttrs["host.name"]; !hasOtelHostname {
    // OTEL didn't set hostname, so check DD settings
```

And the test confirms OTel wins:

```go
t.Run("OTEL host.name has highest priority", func(t *testing.T) {
    // OTEL_RESOURCE_ATTRIBUTES[host.name] always wins, even over DD_HOSTNAME + DD_TRACE_REPORT_HOSTNAME
```

The comment in the docstring is misleading. This is likely intentional behavior (OTel spec says `OTEL_RESOURCE_ATTRIBUTES` wins), but the docstring should be corrected to say "OTel hostname takes precedence over DD hostname" to match the actual behavior.

---

### Minor / Style

**7. `cmp.Or` used for `configValue` zero-value detection**

In `telemetry.go`:

```go
func getMillisecondsConfig(envVar string, defaultMs int) configValue {
    return cmp.Or(
        parseMsFromEnv(envVar),
        configValue{value: defaultMs, origin: telemetry.OriginDefault},
    )
}
```

`cmp.Or` returns the first non-zero value. `configValue{value: 0, origin: OriginEnvVar}` (a valid env var set to `0`) would be treated as "not set" and fall through to the default. This is a subtle bug when a user sets a timeout to `0` (which in practice means "disabled" for some configs). The existing `parseMsFromEnv` returns a zero `configValue{}` on failure, which is correct for error cases, but intentional zero values from env vars would be lost.

For BLRP settings this is non-critical (0ms queue size or timeout would be invalid anyway), but worth documenting or using an explicit `(value, ok)` pattern.

---

**8. `go.sum` references `v0.13.0` while `go.mod` pins to `v0.13.0` but work.sum shows `v0.14.0`**

The `go.work.sum` downgrades the pin from `v0.14.0` entries in the existing workspace sum to add the `v0.13.0` entries in `go.mod`. This inconsistency (`go.mod` at v0.13.0, `go.work.sum` containing both v0.13.0 and v0.14.0 entries) could cause confusion for contributors building against the workspace. This should be unified to the same version.

---

**9. `ForceFlush` acquires a mutex but does not use it to protect the full call**

In `integration.go`:

```go
func ForceFlush(ctx context.Context) error {
    globalLoggerProviderMu.Lock()
    provider := globalLoggerProvider
    globalLoggerProviderMu.Unlock()

    if provider == nil { ... }
    return provider.ForceFlush(ctx)
}
```

Between releasing the lock and calling `provider.ForceFlush(ctx)`, another goroutine could call `ShutdownGlobalLoggerProvider`, which sets `globalLoggerProvider = nil` and shuts down the underlying provider. The `provider.ForceFlush(ctx)` call would then race with shutdown. This is a TOCTOU issue. In practice it is unlikely to be a problem since OTel's `sdklog.LoggerProvider` handles concurrent `ForceFlush` + `Shutdown` gracefully, but the pattern is worth noting.

---

## Positive Observations

- The DD-span-to-OTel-context bridge (`correlation.go`) is well-designed and handles the three cases correctly: no span, DD-only span, and OTel span.
- Comprehensive test coverage for all configuration resolution functions (environment variable priority, fallback defaults, edge cases).
- Retry configuration is sensibly chosen for both HTTP and gRPC.
- The `telemetryExporter` wrapper pattern cleanly separates telemetry from export logic.
- Resource attribute precedence (DD wins over OTEL for service/env/version, OTEL wins for hostname) is well-tested even if the docstring was misleading.
- The singleton pattern with `sync.Once` and the `ShutdownGlobalLoggerProvider` allowing re-initialization is correct.

---

## Checklist Items Not Addressed

- [ ] System tests covering this feature — the PR checklist item is unchecked and no system test PR was linked
- [ ] No `example_test.go` showing lifecycle usage (acknowledged in PR but deferred to external docs)
- [ ] Benchmark for new code — checklist item unchecked (likely not applicable for this type of integration)

---

## Summary of Recommendations

| Severity | Issue | File |
|----------|-------|------|
| Medium | Telemetry counts failed exports as successful | `exporter.go` |
| Medium | `sanitizeOTLPEndpoint` appends path to full signal URLs, violating OTel spec | `exporter.go` |
| Medium | No tracer lifecycle integration, no example code | `integration.go` |
| Low | Misleading docstring: OTEL hostname wins, not DD hostname | `resource.go` |
| Low | `cmp.Or` zero-value logic silently drops env var value of `0` | `telemetry.go` |
| Low | `IsRecording()` always returns `true` for finished DD spans | `correlation.go` |
| Low | TOCTOU in `ForceFlush` | `integration.go` |
| Low | Version inconsistency in `go.mod` vs `go.work.sum` | `go.mod`, `go.work.sum` |
