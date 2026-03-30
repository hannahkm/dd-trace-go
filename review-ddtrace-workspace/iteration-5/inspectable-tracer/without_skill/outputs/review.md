# PR #4512: feat: inspectable tracer

## Summary
This PR introduces a new test infrastructure for dd-trace-go that replaces four existing approaches to mocking/inspecting the tracer in tests. It adds three new packages under `ddtrace/x/`: `agenttest` (a mock APM agent), `tracertest` (test tracer bootstrap functions), and `llmobstest` (LLMObs test collector). The core idea is to use the real tracer with an in-process HTTP transport (no real networking) so tests exercise actual tracing logic rather than mocks. A `Tracer` interface is used, and the old `testtracer` package is deleted. Many existing tests across contrib packages, orchestrion integrations, and llmobs are migrated to the new API.

---

## Blocking

1. **Heavy use of `go:linkname` creates fragile coupling between public test packages and internal implementation**
   - Files: `ddtrace/x/tracertest/tracer.go`, `ddtrace/x/llmobstest/collector.go`
   - `tracertest.Start` is a `go:linkname` alias for `tracer.startInspectableTracer`, `tracertest.Bootstrap` aliases `tracer.bootstrapInspectableTracer`, and `tracertest.StartAgent` aliases `tracer.startAgentTest`. Similarly, `llmobstest` uses `go:linkname` for `withLLMObsInProcessTransport`. If any of these internal function signatures change (parameter order, types, return values), the linked functions break at link time with cryptic errors. This is a maintenance hazard. Since these are test-only APIs, consider instead:
     - Exporting these functions with a `_test` suffix or placing them in `internal/testutil` where they can import the tracer package directly.
     - Or using a well-defined internal interface that the test packages can implement.

2. **`flushHandler` override bypasses production flush logic, masking real bugs**
   - File: `ddtrace/tracer/tracertest.go`, `startInspectableTracer`
   - The test infrastructure replaces `tracer.flushHandler` with a custom function that drains `tracer.out` synchronously and calls `llmobs.FlushSync()`. This is fundamentally different from the production flush path (which is asynchronous and does not drain the channel). Tests using this infrastructure will not catch bugs in the actual flush logic. The comment acknowledges this ("Flushing is ensured to be tested through other E2E tests like system-tests"), but this means the unit test suite has a blind spot for flush-related regressions.

---

## Should Fix

1. **`bootstrapInspectableTracer` sets global tracer state without synchronization guards**
   - File: `ddtrace/tracer/tracertest.go`, `bootstrapInspectableTracer`
   - The function calls `setGlobalTracer(tracer)` and `globalinternal.SetTracerInitialized(true)`, with cleanup that reverses this. If two tests somehow run concurrently (despite the PR noting they cannot), this would race. The cleanup sets `setGlobalTracer(&NoopTracer{})` and `globalinternal.SetTracerInitialized(false)`, but if a test fails before cleanup runs, the global state is left dirty. Consider adding a guard or at minimum a `t.Helper()` annotation and a clear panic if the global tracer is already set.

2. **`agent.Start()` does nothing but set an invalid address**
   - File: `ddtrace/x/agenttest/agent.go`, `Start` method
   - `Start` sets `a.addr = "agenttest.invalid:0"` and returns nil. The address is intentionally invalid because the in-process transport is used. However, this means if someone accidentally uses `agent.Addr()` to make a real HTTP request (e.g., for debugging), it will fail with a confusing error. Consider at least logging or documenting this more prominently.

3. **`handleV1Traces` reads the entire body into memory with `io.ReadAll`**
   - File: `ddtrace/tracer/tracertest.go`, `handleV1Traces`
   - While this is test-only code, there is no size limit. If a test produces a very large trace payload (e.g., stress tests), this could cause OOM. Consider adding a `LimitReader` similar to what `fetchAgentFeatures` uses.

4. **`handleInfo` does not return all fields that the real agent /info endpoint returns**
   - File: `ddtrace/x/agenttest/agent.go`, `handleInfo`
   - The response only includes `endpoints` and `client_drop_p0s`. Missing fields like `span_events`, `span_meta_structs`, `obfuscation_version`, `peer_tags`, `feature_flags`, `config` (statsd_port, default_env) could cause the tracer to behave differently in tests vs production. Consider including all standard fields or making the info response configurable.

5. **`RequireSpan` returns only the first matching span -- this may hide duplicates**
   - File: `ddtrace/x/agenttest/agent.go`, `RequireSpan`
   - The method returns the first span matching the conditions. If there are multiple matching spans (indicating a bug where spans are created twice), tests will pass silently. Consider adding a `RequireUniqueSpan` or at least warning when multiple matches exist.

6. **`toAgentSpan` accesses span fields without holding the span's mutex**
   - File: `ddtrace/tracer/tracertest.go`, `toAgentSpan`
   - The function has `// +checklocksignore` annotation, which suppresses the lock checker. While this is test code and the spans should be finished (and thus not mutated) by the time they reach the agent, this annotation hides potential real races if `toAgentSpan` is ever called on an active span.

7. **The old `testtracer` package is deleted but tests in `llmobs/` and `llmobs/dataset/` and `llmobs/experiment/` are updated to use the new API -- verify no other consumers remain**
   - The deletion of `instrumentation/testutils/testtracer/testtracer.go` is a breaking change for any code that imports it. Ensure no other internal or external consumers exist before merging.

---

## Nits

1. **Package path `ddtrace/x/` is unconventional**
   - The `x/` prefix typically implies "experimental" in Go. If these packages are intended to be the standard test infrastructure going forward, consider a more descriptive path like `ddtrace/testutil/` or `ddtrace/internal/testinfra/`.

2. **`Span.Children` field is declared but never populated**
   - File: `ddtrace/x/agenttest/span.go`, `Children []*Span`
   - The `Children` field exists on the `Span` struct but is never set by any of the trace handlers. Either populate it (by building a span tree after collecting all spans) or remove it to avoid confusion.

3. **`inProcessRoundTripper` does not preserve request body for re-reads**
   - File: `ddtrace/x/agenttest/agent.go`
   - The round-tripper passes `req` directly to `ServeHTTP`. If the handler reads `req.Body`, it is consumed. This is fine for the current use case but worth noting.

4. **`withNoopStats` is used but not shown in the diff**
   - The `withNoopStats()` option is referenced in `startInspectableTracer` but its definition is not visible in the diff. Ensure it is well-documented since test helpers depend on it.

5. **Error handling in `handleV04Traces` and `handleV1Traces` silently returns partial results on decode error**
   - File: `ddtrace/tracer/tracertest.go`
   - Both functions return whatever spans were decoded before the error. This could mask encoding bugs. Consider at least logging the error in test output via `t.Logf`.

6. **The PR description says `testracer.Start` but the code uses `tracertest.Start`**
   - Minor naming discrepancy in the PR description vs actual package name.
