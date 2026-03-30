# Review: PR #4512 - Inspectable tracer test infrastructure

## Summary

This PR introduces a new test infrastructure for dd-trace-go, replacing the existing `testtracer` package with a more modular and deterministic approach. The key components are:

- `ddtrace/x/agenttest` -- A mock APM agent that collects spans in-process via an HTTP round-tripper (no real networking). Provides a builder-pattern `SpanMatch` API for assertions.
- `ddtrace/x/tracertest` -- Functions to create an inspectable tracer backed by the mock agent. Uses `go:linkname` to call unexported tracer internals.
- `ddtrace/x/llmobstest` -- A collector for LLMObs spans/metrics using the same in-process transport pattern.
- `ddtrace/tracer/tracertest.go` -- Internal helpers including a stronger flush handler that drains the `tracer.out` channel before flushing, eliminating timeout-based polling.

The old `instrumentation/testutils/testtracer` package is deleted. Tests across contrib packages and LLMObs are migrated to the new API.

## Applicable guidance

- style-and-idioms.md (all Go code)
- concurrency.md (flush handler, channel draining, goroutine lifecycle)
- performance.md (flush handler touches trace writer internals)

---

## Blocking

1. **Heavy use of `go:linkname` to access unexported tracer internals from `ddtrace/x/` packages** (`tracertest/tracer.go:30,37,44`). Three functions use `go:linkname`: `Start`, `Bootstrap`, and `StartAgent`. This creates a fragile coupling between the test packages and the tracer's internal API surface. If the linked function signatures change, the build breaks silently or at link time with cryptic errors. The Go team has been progressively tightening `go:linkname` restrictions. Per style-and-idioms.md on avoiding unnecessary indirection, consider whether these functions could instead be exported as `tracer.StartForTest` / `tracer.BootstrapForTest` with a build tag or test-only file, or if the `x/` package pattern genuinely adds enough value to justify `go:linkname`.

2. **`llmobstest` also uses `go:linkname` for `withLLMObsInProcessTransport`** (`llmobstest/collector.go:64-65`). Same concern as above. This links to an unexported function in the tracer package. If the function is needed by test packages, consider exporting it with a clear test-only intent (e.g., in a `_test.go` file or with a test build tag).

3. **The custom `flushHandler` in `startInspectableTracer` directly accesses `tracer.out` channel and `agentTraceWriter` internals** (`tracertest.go:86-109`). The flush handler drains `tracer.out` via a select/default loop, calls `sampleChunk`, `traceWriter.add`, `traceWriter.flush`, and then does a type assertion `tracer.traceWriter.(*agentTraceWriter).wg.Wait()`. This tightly couples the test infrastructure to the tracer's internal implementation details. If the trace writer implementation changes (e.g., a different writer type, or the `out` channel is replaced), this will silently break. The comment acknowledges this is "kind of a hack." Consider adding an internal interface or hook that the test infrastructure can use without reaching into implementation details.

4. **`toAgentSpan` accesses span fields without holding `s.mu`** (`tracertest.go:8-33`). The function reads `span.spanID`, `span.traceID`, `span.meta`, `span.metrics`, etc. without acquiring the span's mutex. The `+checklocksignore` annotation suppresses the `checklocks` analyzer, but the underlying data race risk remains. This function is called from the flush handler which drains the `out` channel -- at that point the span should be finished and not concurrently mutated, but this is an implicit contract. Per concurrency.md, span field access after `Finish()` should go through the span's mutex to be safe. Add a comment explaining why the lock is not needed here (if the span is guaranteed to be immutable at this point), or acquire the lock.

## Should fix

1. **`Agent` interface in `agenttest` has `Start` returning `error` but the implementation is a no-op** (`agenttest/agent.go:87,180-183`). `Start` sets `a.addr = "agenttest.invalid:0"` and returns nil. The error return is unused infrastructure. If this is forward-looking API design (e.g., for a future network-based agent), that is speculative API surface. Per the universal checklist: "Don't add unused API surface." Consider removing the error return or documenting why it exists.

2. **Duplicated `inProcessRoundTripper` type** (`agenttest/agent.go:172-178`, `llmobstest/collector.go:76-82`). Both `agenttest` and `llmobstest` define identical `inProcessRoundTripper` structs. Extract this into a shared internal package to avoid duplication. Per the checklist: "Extract shared/duplicated logic."

3. **`bootstrapInspectableTracer` sets global tracer state but does not reset all global state on cleanup** (`tracertest.go:56-69`). The cleanup sets the global tracer to `NoopTracer` and resets `TracerInitialized`, but does not clean up other global state (like appsec, which is started on line 114 but only cleaned up for llmobs). Per concurrency.md: "Global state must reset on tracer restart." Ensure `appsec.Stop()` is called in cleanup if `appsec.Start` was called.

4. **`handleV04Traces` and `handleV1Traces` silently swallow errors** (`tracertest.go:40-60`). Both functions return partial results on decode errors without logging or flagging the failure. In test infrastructure, silent data loss makes debugging very difficult. Consider at least logging decode errors, or returning them alongside the spans.

5. **`RequireSpan` diagnostic output in the agent uses `fmt.Appendf` which is available only in Go 1.21+** (`agenttest/agent.go:117`). Verify this is compatible with the repo's minimum Go version. If the repo supports Go < 1.21, use `fmt.Sprintf` with string concatenation instead.

6. **`SpanMatch.Tag` uses `==` comparison for `any` type** (`agenttest/span.go:30-36`). For complex tag values (maps, slices), `==` on `any` does not work correctly. Consider using `reflect.DeepEqual` or documenting that `Tag` only works for comparable types.

## Nits

1. **Package documentation for `ddtrace/x/` is well-written** with clear examples in the godoc comments. Good.

2. **The `goto drained` pattern in the flush handler** (`tracertest.go:99-102`) is functional but uncommon in Go. A labeled break or a helper function would be more idiomatic.

3. **`CountSpans` uses `a.mu.Lock()` instead of `a.mu.RLock()`** (`agenttest/agent.go:131-134`). Since this is a read-only operation, use `RLock`/`RUnlock` for consistency and to allow concurrent reads.

4. **Copyright year 2026 in new files** -- presumably correct for when this code was written, but worth double-checking.

The overall architecture is a significant improvement over the old `testtracer` -- the in-process transport eliminates network flakiness, the stronger flush handler eliminates timeout polling, and the builder-pattern `SpanMatch` API provides better diagnostics on assertion failures. The explicit decision not to expose span slices (documented in `agenttest` godoc) is a good design choice to prevent order-dependent test flakiness.
