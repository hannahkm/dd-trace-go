# PR #4489: feat(openfeature): add flag evaluation tracking via OTel Metrics

## Summary

This PR adds flag evaluation metrics tracking to the OpenFeature provider using the OTel Metrics API (Metrics Platform path per the RFC). A new `flagEvalHook` implements the OpenFeature `Hook` interface, using the `Finally` stage to record a `feature_flag.evaluations` counter with attributes: `feature_flag.key`, `feature_flag.result.variant`, `feature_flag.result.reason`, and `error.type`. The metrics are emitted via a dedicated `MeterProvider` created through dd-trace-go's OTel metrics support. When `DD_METRICS_OTEL_ENABLED` is not `true`, the provider is a noop.

**Files changed:** `openfeature/flageval_metrics.go` (new), `openfeature/flageval_metrics_test.go` (new), `openfeature/provider.go`, `openfeature/provider_test.go`

---

## Blocking

None identified.

---

## Should Fix

### 1. `newFlagEvalMetrics` error is logged but hook is still created with nil metrics

In `newDatadogProvider()`:
```go
metrics, err := newFlagEvalMetrics()
if err != nil {
    log.Error("openfeature: failed to create flag evaluation metrics: %v", err.Error())
}
// ...
flagEvalHook: newFlagEvalHook(metrics),
```

When `err != nil`, `metrics` will be `nil`, and `newFlagEvalHook(nil)` creates a hook with a nil `metrics` field. The `Finally` method does have a `nil` guard (`if h.metrics == nil { return }`), so this won't crash. However, the hook is still added to the `Hooks()` slice, meaning OpenFeature will invoke `Finally` on every evaluation even though it will immediately return. While the overhead is minimal, it would be cleaner to not add the hook at all when metrics creation fails:

```go
if metrics != nil {
    p.flagEvalHook = newFlagEvalHook(metrics)
}
```

This also avoids the hook appearing in `Hooks()` when it does nothing.

### 2. Shutdown error is silently discarded

In `ShutdownWithContext`:
```go
if p.flagEvalHook != nil && p.flagEvalHook.metrics != nil {
    _ = p.flagEvalHook.metrics.shutdown(ctx)
}
```

The error from `shutdown` is discarded with `_`. If the meter provider shutdown fails (e.g., due to context timeout), this should at least be logged, similar to how other shutdown errors are handled. At minimum, it could contribute to the `err` variable sent on the `done` channel, or be logged separately.

### 3. No `TargetProviderNotReadyCode` or `InvalidContextCode` error mapping

The `errorCodeToTag` function handles `FlagNotFoundCode`, `TypeMismatchCode`, `ParseErrorCode`, and a `default` catch-all returning `"general"`. The OpenFeature spec also defines `TargetingKeyMissingCode`, `ProviderNotReadyCode`, and `InvalidContextCode`. While the `default` branch handles these, explicit mappings would provide more useful metric tags for debugging. Consider whether these error codes are expected in the Datadog provider's usage and whether they warrant distinct metric values.

### 4. Missing `TestShutdownClean` test in the diff

The PR description mentions `TestShutdownClean` passing, but this test is not present in the diff. If it existed before, that's fine. If it's expected to be part of this PR, it appears to be missing.

---

## Nits

### 1. `log.Error` format string inconsistency

```go
log.Error("openfeature: failed to create flag evaluation metrics: %v", err.Error())
```

Using `err.Error()` with `%v` is redundant. Either use `%v` with `err` directly, or `%s` with `err.Error()`:
```go
log.Error("openfeature: failed to create flag evaluation metrics: %v", err)
```

### 2. Test helper `makeDetails` constructs `InterfaceEvaluationDetails` with deeply nested initialization

The `makeDetails` helper works fine but the triple-nested struct initialization is a bit hard to read. This is a minor readability concern and the current form is acceptable.

### 3. `metricUnit` uses UCUM notation

The metric unit is `{evaluation}` which follows the UCUM annotation syntax (used by OTel). This is correct per spec but worth noting for anyone unfamiliar with the convention.

### 4. Hardcoded 10-second export interval

The export interval is hardcoded to `10 * time.Second`:
```go
mp, err := ddmetric.NewMeterProvider(
    ddmetric.WithExportInterval(10 * time.Second),
)
```

This matches the RFC's recommendation to align with EVP track flush cadence, but it is not configurable. For a first implementation this is fine, but consider whether it should be configurable via an environment variable or provider config option in the future.
