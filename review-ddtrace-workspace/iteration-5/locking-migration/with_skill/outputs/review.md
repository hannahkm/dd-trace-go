# Review: PR #4359 — Locking migration: sync.Mutex -> locking.Mutex in ddtrace/tracer

## Summary

This PR migrates all `sync.Mutex` and `sync.RWMutex` usage in `ddtrace/tracer/` to `internal/locking.Mutex` and `internal/locking.RWMutex`. It also adds golangci-lint `forbidigo` rules to enforce the new convention (with exemptions for tests, internal/locking itself, and non-`ddtrace/tracer` paths). Beyond the mechanical replacement, the PR makes significant structural changes to `spancontext.go`'s `finishedOneLocked` to fix lock ordering between span and trace mutexes — eliminating `withLockIf`, removing `defer t.mu.Unlock()`, and manually managing lock/unlock to avoid holding `trace.mu` while acquiring `span.mu`.

## Reference files consulted

- style-and-idioms.md (always)
- concurrency.md (mutex discipline, checklocks, lock ordering, callbacks under lock)
- performance.md (lock contention in hot paths, minimize critical section scope)

## Findings

### Blocking

1. **Race in `finishedOneLocked` partial flush: `t.setTraceTagsLocked(fSpan)` acquires `t.mu.RLock` but `t.mu` was just released** (`spancontext.go:714-717`). After the partial flush path releases `t.mu.Unlock()` at line 706, it later re-acquires `t.mu.RLock()` at line 715 to call `setTraceTagsLocked(fSpan)`. Between the unlock and re-lock, another goroutine could modify `t.tags` or `t.propagatingTags` (e.g., another span finishing concurrently could trigger `finishedOneLocked` and modify trace state). The values read during `setTraceTagsLocked` could be inconsistent with the snapshot taken earlier (e.g., `priority`, `willSend`, `needsFirstSpanTags` were all captured before unlock). If `t.spans[0]` changes between the unlock and the RLock (because another goroutine modifies leftoverSpans or a new span is added), the `needsFirstSpanTags` check based on the old `t.spans[0]` could be stale. This is a subtle but real concern in the partial flush path.

2. **`s.finished = true` moved inside `t.mu.Lock` but the old code set it under `s.mu` (held by caller)** (`spancontext.go:621-622`). Previously, `s.finished = true` was set at the top of `finishedOneLocked` while the caller held `s.mu`. Now it is set after the `t.mu.Lock()` acquisition. This is functionally fine since `s.mu` is still held by the caller and the `s.finished` check at line 618 prevents double-finish. However, the new guard `if s.finished { t.mu.Unlock(); return }` is a good addition that prevents double-counting, which the old code did not have. This is actually an improvement.

   **On reflection, the double-finish guard is a net positive.** Not a concern.

3. **`t.root.setMetricLocked(keySamplingPriority, *t.priority)` changed to `s.setMetricLocked(keySamplingPriority, *t.priority)`** (`spancontext.go:644`). The old code set the sampling priority on `t.root`, the new code sets it on `s` (the current span being finished). When `s == t.root`, these are equivalent. When `s != t.root`, the old code set the metric on root (which was correct — sampling priority belongs on root), while the new code sets it on whichever span happened to finish last to complete the trace. This seems like a behavioral change that may be incorrect: if the root finishes first but non-root spans finish later to complete the trace, the priority metric would be set on a non-root span. However, looking more carefully at the condition (`if t.priority != nil && !t.locked`), this block runs when priority hasn't been locked yet. The root finishing would lock priority (line 645: `t.locked = true`). So the only way to reach this with `s != t.root` is if priority was set but root hasn't finished yet... which means the priority should indeed go on root. **This change may be incorrect** — unless there is a guarantee that this code path only executes when `s == t.root`.

### Should fix

1. **Manual `t.mu.Unlock()` calls before every return path are error-prone** (`spancontext.go:617,621,628,660,668,706`). The old code used `defer t.mu.Unlock()` which is safe against panics and guarantees unlock. The new code has six explicit `t.mu.Unlock()` calls spread across different return paths. While this is intentional (to release the trace lock before acquiring span locks, following the lock ordering invariant), it is fragile: a future code change that adds a new return path or moves code could forget to unlock. Consider extracting the critical section into a helper that returns the data needed, then doing post-unlock work with the returned data. This would keep `defer` while maintaining lock ordering. At minimum, add a comment at the function entry noting the manual unlock pattern and why `defer` is not used.

2. **Test changes in `abandonedspans_test.go` replace shared `tg` with per-subtest `tg` and add `assert.Eventually`** (`abandonedspans_test.go`). The shared `tg` with `tg.Reset()` between subtests was technically a race if subtests ran in parallel (they don't by default, but the pattern is fragile). Moving to per-subtest `tg` is correct. The added `assert.Eventually` calls are also good — they address the inherent timing issue where the ticker may not have fired yet. However, the `assert.Len(calls, 1)` assertion after `assert.Eventually` is redundant since `Eventually` already checked `len(calls) == 1`. This is a nit.

3. **`finishChunk` method removed, inlined as `tr.submitChunk`** (`spancontext.go`). The old `finishChunk` method called `tr.submitChunk` and reset `t.finished`. The new code inlines the `submitChunk` call and resets `t.finished` separately. The test `TestTraceFinishChunk` was renamed to `TestSubmitChunkQueueFull` and simplified. This is clean — the removed method was one line of actual logic. Good simplification.

4. **Lint rules only apply to `ddtrace/tracer/` via `path-except`** (`.golangci.yml:38-41`). The `forbidigo` rules for `sync.Mutex` and `sync.RWMutex` are scoped to `ddtrace/tracer/` only (the `path-except: "^ddtrace/tracer/"` line means the suppression applies to everything *except* tracer). This is a reasonable first step but means contrib packages and other internal packages can still use `sync.Mutex` directly. The README migration checklist items for Phase 2/3 have been removed — is the plan to expand the lint scope later? Consider leaving a TODO comment in the lint config about future expansion.

### Nits

1. **Comment on `finishedOneLocked` says "TODO: Add checklocks annotation"** (`spancontext.go:603`). This is good to have as a reminder, but consider filing it as an issue so it doesn't get lost.

2. **`format/go` Makefile target added** (`Makefile:84-86`). This is a nice developer ergonomics addition. The README.md and scripts/README.md are updated consistently.

3. **The README.md migration checklist section was removed entirely** (`internal/locking/README.md`). The checklist tracked the multi-phase rollout. Since Phase 1 and the tracer-level Phase 2 are now done, removing it makes sense. But the remaining "Integration with Static Analysis" section may benefit from a note about the lint enforcement now being active.

## Overall assessment

This is a significant and carefully thought-out PR. The mechanical `sync.Mutex` -> `locking.Mutex` replacement is straightforward, but the real substance is the lock ordering fix in `finishedOneLocked`. The change from `defer t.mu.Unlock()` to manual unlock-before-relock is motivated by the correct concern (avoiding holding trace.mu while acquiring span.mu during partial flush). The main risk is the sampling priority target change (`t.root` -> `s`) which may be a behavioral regression, and the general fragility of the manual unlock pattern. The test improvements (per-subtest statsd clients, `assert.Eventually`) are good housekeeping.
