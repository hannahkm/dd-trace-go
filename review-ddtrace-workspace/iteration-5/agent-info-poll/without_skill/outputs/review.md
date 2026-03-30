# PR #4451: feat(tracer): periodically poll agent /info endpoint for dynamic capability updates

## Summary
This PR adds periodic polling (every 5 seconds by default) of the Datadog Agent's `/info` endpoint so the tracer can dynamically pick up agent capability changes (peer tags, span events, stats collection flags, etc.) without requiring a restart. The implementation wraps `agentFeatures` in an `atomicAgentFeatures` type backed by `atomic.Pointer[agentFeatures]` for lock-free reads on the hot path, and uses a CAS-loop `update()` method for safe concurrent writes. Static fields (transport URL, statsd port, feature flags, etc.) are preserved from startup, while dynamic fields are refreshed on each poll.

---

## Blocking

1. **`refreshAgentFeatures` spawns an unbounded goroutine that can leak if `fetchAgentFeatures` blocks**
   - File: `ddtrace/tracer/tracer.go`, `refreshAgentFeatures` method
   - The method creates a background goroutine (`go func() { select ... }`) to propagate cancellation from `t.stop` to the context. However, if `fetchAgentFeatures` completes normally, the `defer cancel()` fires and the goroutine exits via `case <-ctx.Done()`. The problem arises if `fetchAgentFeatures` hangs for longer than the poll interval: the next `refreshAgentFeatures` call spawns another goroutine while the previous one is still alive. Over time with a slow/unreachable agent, this accumulates goroutines. Consider using `context.WithTimeout` with a deadline shorter than the poll interval instead of the unbounded approach, or use a single long-lived cancellable context.

2. **`peerTags` is defensively cloned from `newFeatures` but `featureFlags` is cloned from `current` -- inconsistent treatment of dynamic vs static**
   - File: `ddtrace/tracer/tracer.go`, inside the `update()` closure in `refreshAgentFeatures`
   - `f.peerTags = slices.Clone(newFeatures.peerTags)` takes the *new* peer tags (treating them as dynamic), but `f.featureFlags = maps.Clone(current.featureFlags)` takes the *current/startup* feature flags (treating them as static). The test `TestRefreshAgentFeaturesPreservesStaticFields` confirms feature flags are expected to be static. However, the PR description says "Only fields safe to update at runtime (DropP0s, Stats, peerTags, spanEventsAvailable, obfuscationVersion) are refreshed." If `peerTags` is dynamic, then the comment and code are consistent, but the obfuscator config in `newUnstartedTracer` reads feature flags once at startup and never refreshes -- meaning feature flags changes would require a restart anyway. This inconsistency should be explicitly documented in a code comment clarifying which fields are static vs dynamic and *why*.

---

## Should Fix

1. **No timeout on the `/info` HTTP request**
   - File: `ddtrace/tracer/tracer.go`, `refreshAgentFeatures`
   - The context passed to `fetchAgentFeatures` is only cancelled when the tracer stops, not on any timeout. If the agent is slow to respond, the poll goroutine blocks indefinitely (or until the next ticker fires). Add a `context.WithTimeout` (e.g., 3 seconds) to bound each poll attempt. This would also address the goroutine accumulation concern in Blocking #1.

2. **`update()` CAS loop is unbounded with no backoff**
   - File: `ddtrace/tracer/option.go`, `atomicAgentFeatures.update` method
   - The CAS loop retries without any backoff or limit. While concurrent writes should be rare (only polling writes), if something goes wrong, this could busy-loop. Consider adding a maximum retry count or a brief `runtime.Gosched()` between retries.

3. **The concentrator reads `peerTags` on every call to `newTracerStatSpan`**
   - File: `ddtrace/tracer/stats.go`, line `PeerTags: c.cfg.agent.load().peerTags`
   - This atomic load happens on every span that gets stats computed. While `atomic.Pointer.Load` is fast, the previous code read `peerTags` from a plain struct field (zero overhead). For high-throughput tracers, this adds per-span overhead. Consider caching the peer tags in the concentrator and refreshing them periodically or when the agent features change, rather than loading atomically on every span.

4. **Missing benchmark for the atomic load hot path**
   - The PR checklist acknowledges no benchmark was added. Since `c.agent.load()` is now called on the hot path (every span start in `StartSpan`, every stat computation in `newTracerStatSpan`), a benchmark comparing before/after would help quantify any regression and serve as a regression test.

5. **`io.Copy(io.Discard, resp.Body)` on 404 but not on other error status codes**
   - File: `ddtrace/tracer/option.go`, `fetchAgentFeatures`
   - The response body is drained on 404 for connection reuse, but when the status is non-200 and non-404, the body is not drained before the deferred `resp.Body.Close()`. This prevents HTTP connection reuse for those cases. Add `io.Copy(io.Discard, resp.Body)` before returning the error for unexpected status codes.

6. **The obfuscator is still configured once at startup and never refreshed**
   - File: `ddtrace/tracer/tracer.go`, `newUnstartedTracer`
   - The obfuscator config reads `c.agent.load()` feature flags once. Even though feature flags are now classified as static, the fact that they are wrapped in an atomic load suggests the author may have intended them to be refreshable. If the intent is truly static, this code should use the `af` local variable from `loadAgentFeatures` instead of going through the atomic. If the intent is dynamic, the obfuscator needs a mechanism to reconfigure.

---

## Nits

1. **Comment says "Goroutine lifetime bounded by defer cancel()" but the goroutine outlives the function if the HTTP request blocks**
   - File: `ddtrace/tracer/tracer.go`, `refreshAgentFeatures`
   - The comment `// Goroutine lifetime bounded by defer cancel() above; no wg tracking needed.` is misleading. If `fetchAgentFeatures` blocks (e.g., agent is slow), the goroutine remains alive until either `t.stop` fires or the context is cancelled. The comment should be clarified.

2. **Inconsistent error handling style in `fetchAgentFeatures`**
   - File: `ddtrace/tracer/option.go`
   - The function returns wrapped errors (`fmt.Errorf("creating /info request: %w", err)`) for most cases but returns `errAgentFeaturesNotSupported` as a sentinel. This is fine architecturally, but consider wrapping the sentinel too so callers can use `errors.Is` while still getting context (e.g., `fmt.Errorf("agent /info: %w", errAgentFeaturesNotSupported)`).

3. **`agentURL.JoinPath("info")` could produce a double-slash if agentURL has a trailing slash**
   - File: `ddtrace/tracer/option.go`, `fetchAgentFeatures`
   - Depending on how `agentURL` is constructed, `JoinPath("info")` may or may not handle trailing slashes correctly. The original code used `fmt.Sprintf("%s/info", agentURL)`. Verify that `JoinPath` handles edge cases (e.g., `http://host:8126/` vs `http://host:8126`).

4. **`1<<20` LimitReader magic number**
   - File: `ddtrace/tracer/option.go`, `io.LimitReader(resp.Body, 1<<20)`
   - The 1 MiB limit is reasonable but would benefit from a named constant for readability (e.g., `const maxAgentInfoResponseSize = 1 << 20`).

5. **Test helper `withAgentInfoPollInterval` is unexported but could be useful for other test files**
   - File: `ddtrace/tracer/poll_agent_info_test.go`
   - Since it is a `StartOption`, it works as a test helper. This is fine for now, but if other test files need to control poll interval, consider moving it to a shared test helper file.
