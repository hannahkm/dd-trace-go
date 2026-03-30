# Review: PR #4451 - Periodic agent /info polling

## Summary

This PR introduces periodic polling of the trace-agent's `/info` endpoint to refresh agent capabilities (like `DropP0s`, `Stats`, `spanEventsAvailable`, `peerTags`, `obfuscationVersion`) without requiring a tracer restart. It replaces the direct `agentFeatures` struct field on `config` with an `atomicAgentFeatures` wrapper using `atomic.Pointer` for lock-free reads on the hot path. Static fields baked into components at startup (transport URL, statsd port, evpProxyV2, etc.) are preserved across polls, while dynamic fields are updated.

## Applicable guidance

- style-and-idioms.md (all Go code)
- concurrency.md (atomics, shared state, goroutine lifecycle)
- performance.md (hot path reads in StartSpan, stats computation)

---

## Blocking

1. **`refreshAgentFeatures` spawns a fire-and-forget goroutine for cancellation that is not tracked by any waitgroup** (`tracer.go:858-867`). The goroutine listens for `t.stop` to cancel the HTTP request context, but its lifetime is only bounded by `defer cancel()` from the parent. If `fetchAgentFeatures` returns quickly (e.g., the request completes before `t.stop`), the goroutine will be racing to select on `ctx.Done()` which fires from the deferred `cancel()`. This is technically safe but fragile. More importantly, if `Stop()` is called while `refreshAgentFeatures` is mid-flight, the goroutine for cancellation may briefly leak during the CAS-loop in `update()`. Consider using `http.NewRequestWithContext` with a context derived from `t.stop` directly (per concurrency.md: "use `http.NewRequestWithContext` tied to a cancellation signal so it doesn't block shutdown") instead of spawning a separate goroutine. For example, store a `gocontext.Context` and `cancel` on the tracer struct that is cancelled by `Stop()`, and pass that to `fetchAgentFeatures`.

2. **`c.cfg.agent.load().peerTags` is called on every span in `newTracerStatSpan`** (`stats.go:180`). The concentrator calls `c.cfg.agent.load().peerTags` for every span that goes through stats computation. This is a hot path (per performance.md: "Don't call TracerConf() per span"). While `atomic.Pointer.Load()` is cheaper than a mutex, it still incurs an atomic load + pointer dereference + slice copy for every span. Consider caching `peerTags` in the concentrator and refreshing it on a less frequent cadence, or having the poll goroutine push updated values to the concentrator rather than having the concentrator pull on every span.

## Should fix

1. **`update()` CAS loop comment says "fn must be a pure transform" but `maps.Clone` and `slices.Clone` inside the closure allocate on each retry** (`tracer.go:879-894`). While this is functionally correct (the allocations are local and don't escape), it is wasteful under contention. The comment claims purity, but the defensive clones mean each CAS retry allocates new backing arrays. Under normal operation there should be minimal contention (only the poll goroutine writes), so this is not a correctness issue, but the comment should be more precise about what "pure" means here (no external side effects, but may allocate).

2. **`peerTags` is marked as a dynamic field in `refreshAgentFeatures` but is cloned from `newFeatures`, while other dynamic fields are also taken from `newFeatures`** (`tracer.go:889`). The line `f.peerTags = slices.Clone(newFeatures.peerTags)` clones from the fresh snapshot, which is correct for a dynamic field. However, the code pattern is inconsistent -- all other dynamic fields are implicitly carried over from `f` (which starts as a copy of `newFeatures`), while `peerTags` gets an explicit clone. The explicit clone is defensive but could confuse future maintainers. Add a comment explaining that the explicit clone is necessary because slices share backing arrays on shallow copy.

3. **`shouldObfuscate()` calls `c.cfg.agent.load()` on each invocation** (`stats.go:196-197`). This is called from `flushAndSend` which runs periodically (not per-span), so it is less critical, but the pattern of loading atomic features repeatedly in the same function without hoisting to a local variable is inconsistent with the approach used in `startTelemetry` and `canComputeStats`. Hoist the load to a local for consistency and to avoid the minor risk of reading two different snapshots within the same flush.

4. **`defaultAgentInfoPollInterval` is 5 seconds which may be aggressive for production** (`tracer.go:494`). The comment says "polls the agent's /info endpoint for capability updates" but doesn't explain why 5 seconds was chosen. Per style-and-idioms.md, explain "why" for non-obvious config: 5s means ~720 requests/hour to the local trace-agent. If the typical agent config change cadence is on the order of minutes, a 30s or 60s interval might be more appropriate. Add a rationale comment.

5. **No test for tracer restart cycle preserving correct poll behavior** (concurrency.md: "Global state must reset on tracer restart"). The `pollAgentInfo` goroutine is tracked by `t.wg` and stopped via `t.stop`, which looks correct. However, there is no test verifying that `Start()` -> `Stop()` -> `Start()` correctly starts a fresh poll goroutine with no stale state. The `atomicAgentFeatures` on the new `config` should be fresh, but this should be explicitly tested since restart-related bugs are a recurring issue in this repo.

## Nits

1. **`fetchAgentFeatures` uses `agentURL.JoinPath("info")` which may produce different URL formatting than the original `fmt.Sprintf("%s/info", agentURL)`** (`option.go:149`). `JoinPath` handles trailing slashes differently. This is likely fine but worth noting if any tests depend on exact URL matching.

2. **The `infoResponse` struct is declared inside `fetchAgentFeatures`** (`option.go:174-177`). This is fine for encapsulation, but since it was previously inside `loadAgentFeatures` and now `loadAgentFeatures` delegates to `fetchAgentFeatures`, the struct moved but the pattern is preserved. No action needed.

3. **Test `TestPollAgentInfoUpdatesFeaturesDynamically` uses `assert.Eventually` with `10*pollInterval` timeout** (`poll_agent_info_test.go:491-494`). With `pollInterval = 20ms`, the timeout is 200ms. This is tight and could be flaky under CI load. Consider a slightly more generous timeout like `2*time.Second` while keeping the poll interval at 20ms.

4. **`io.Copy(io.Discard, resp.Body)` on 404 response** (`option.go:165`). Good practice for connection reuse. The `//nolint:errcheck` comment is appropriate.

The code overall is well-structured. The separation between static (startup-frozen) and dynamic (poll-refreshed) agent features is clear, the CAS-based atomic update avoids locks on the hot path, and the test coverage is thorough with tests for dynamic updates, error retention, shutdown, and 404 handling.
