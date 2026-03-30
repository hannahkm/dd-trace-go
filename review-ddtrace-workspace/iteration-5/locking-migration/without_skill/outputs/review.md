# PR #4359: chore(ddtrace/tracer): migrate to locking.*Mutex to enable dynamic lock checks

## Summary

This PR has three parts:
1. **Lint enforcement**: Adds `forbidigo` rules to `.golangci.yml` to forbid direct `sync.Mutex` and `sync.RWMutex` usage outside of the `internal/locking` package, enforcing use of `locking.Mutex` and `locking.RWMutex` wrappers throughout the tracer.
2. **Mechanical migration**: Replaces `sync.Mutex`/`sync.RWMutex` with `locking.Mutex`/`locking.RWMutex` across core tracer packages (`sampler.go`, `rules_sampler.go`, `payload.go`, `option.go`, `dynamic_config.go`, `remote_config.go`, `tracer.go`, `writer.go`, `spancontext.go`, and test files).
3. **Deadlock fix**: Refactors `trace.finishedOneLocked()` in `spancontext.go` to fix a discovered deadlock by changing lock ordering and removing `defer t.mu.Unlock()` in favor of explicit unlock-before-lock patterns.

**Key files changed:** `.golangci.yml`, `ddtrace/tracer/spancontext.go`, `ddtrace/tracer/span.go`, `ddtrace/tracer/sampler.go`, `ddtrace/tracer/rules_sampler.go`, `ddtrace/tracer/payload.go`, `ddtrace/tracer/option.go`, `ddtrace/tracer/dynamic_config.go`, `ddtrace/tracer/remote_config.go`, `ddtrace/tracer/tracer.go`, `ddtrace/tracer/writer.go`, `ddtrace/tracer/tracer_test.go`, `ddtrace/tracer/spancontext_test.go`, `ddtrace/tracer/abandonedspans_test.go`

---

## Blocking

### 1. `finishedOneLocked`: Setting `s.finished = true` moved inside `t.mu.Lock` -- potential semantic issue

Previously:
```go
func (t *trace) finishedOneLocked(s *Span) {
    t.mu.Lock()
    defer t.mu.Unlock()
    s.finished = true    // set unconditionally
    ...
}
```

Now:
```go
func (t *trace) finishedOneLocked(s *Span) {
    t.mu.Lock()
    if t.full { t.mu.Unlock(); return }
    if s.finished { t.mu.Unlock(); return }  // NEW guard
    s.finished = true
    ...
}
```

The new `s.finished` guard prevents double-finishing a span, which is good. However, `s.finished` is a field on the span, and the function's documented invariant is "The caller MUST hold s.mu." The `s.finished` check happens while `t.mu` is also held, which is correct for the new lock ordering (span.mu -> trace.mu). But if `s.finished` was previously set by a different code path that doesn't go through `finishedOneLocked`, this guard could silently swallow finish calls. Verify that all paths that set `s.finished = true` go through this function.

### 2. `setTraceTagsLocked` called with only `t.mu.RLock` during partial flush

In the partial flush path:
```go
t.mu.Unlock()
// ... acquire fSpan lock ...
if needsFirstSpanTags {
    t.mu.RLock()
    t.setTraceTagsLocked(fSpan)
    t.mu.RUnlock()
}
```

`setTraceTagsLocked` modifies `fSpan` (setting tags on it), not `t`. However, it reads from `t.tags` and `t.propagatingTags`. The RLock on `t.mu` is correct for reading trace-level tags. But between `t.mu.Unlock()` and `t.mu.RLock()`, another goroutine could modify `t.tags` or `t.propagatingTags`. This is a window where the trace-level tags could change, potentially causing `setTraceTagsLocked` to see inconsistent state. Assess whether any concurrent path modifies `t.tags`/`t.propagatingTags` after a span has started finishing.

### 3. Sampling priority set on `s` instead of `t.root` for root span case

The code changes:
```diff
-t.root.setMetricLocked(keySamplingPriority, *t.priority)
+s.setMetricLocked(keySamplingPriority, *t.priority)
```

This change is at the point where `t.priority != nil`. The original code set the sampling priority on `t.root` regardless of which span was finishing. The new code sets it on `s` (the span being finished). This is only correct if `s == t.root` at this point, or if the intent is to always set sampling priority on whichever span finishes (which would be incorrect for non-root spans). Looking at the surrounding code: this executes when `t.priority != nil`, which happens when priority sampling is set. The comment says "after the root has finished we lock down the priority" but the guard checks `t.priority != nil`, not `s == t.root`. If a non-root span finishes with priority set, this now puts the sampling priority metric on a non-root span instead of the root. This could be a correctness bug if the root has not yet been locked and the priority changes later.

---

## Should Fix

### 1. Multiple early-return unlock pattern is error-prone

The refactored `finishedOneLocked` has multiple `t.mu.Unlock(); return` patterns:

```go
t.mu.Lock()
if t.full {
    t.mu.Unlock()
    return
}
if s.finished {
    t.mu.Unlock()
    return
}
// ... more code ...
if tr == nil {
    t.mu.Unlock()
    return
}
// ... more code ...
if len(t.spans) == t.finished {
    // ... unlock and return
}
if !doPartialFlush {
    t.mu.Unlock()
    return
}
// ... partial flush path ... t.mu.Unlock()
```

This replaces a single `defer t.mu.Unlock()` with 5+ explicit unlock points. While each individual path looks correct, this is fragile -- any future modification that adds a new return path or panics before unlocking will cause a deadlock or leaked lock. Consider restructuring to minimize unlock points, perhaps by extracting the work-after-unlock into separate functions that are called after a single unlock point.

### 2. `finishChunk` method removed, inlined as `tr.submitChunk`

The `finishChunk` method was removed and its body inlined. The old `finishChunk` also reset `t.finished = 0`, which is now done explicitly at each call site. This is fine but the duplication of `t.finished = 0` at two separate code paths (full flush and partial flush) is easy to miss. A comment at each site explaining why the reset is needed would help.

### 3. Test flakiness fix in `abandonedspans_test.go` uses `Eventually`

The test fix adds `assert.Eventually` to wait for the ticker to fire:
```go
assert.Eventually(func() bool {
    calls := tg.GetCallsByName("datadog.tracer.abandoned_spans")
    return len(calls) == 1
}, 2*time.Second, tickerInterval/10)
```

This is a good fix for the flaky test, but the `2*time.Second` timeout is relatively generous for a `100ms` ticker interval. If the ticker reliably fires within ~200ms, a 500ms timeout would be sufficient and make the test fail faster if there is a real regression. The current timeout is fine for CI stability though.

### 4. `withLockIf` removal

The `withLockIf` helper on `Span` is removed:
```go
func (s *Span) withLockIf(condition bool, f func()) {
    if condition { s.mu.Lock(); defer s.mu.Unlock() }
    f()
}
```

This was used in the partial flush path to conditionally lock a span. The replacement explicitly checks and locks:
```go
if !currentSpanIsFirstInChunk {
    fSpan.mu.Lock()
    defer fSpan.mu.Unlock()
}
```

This is clearer and better for lock analysis tools. Good change.

---

## Nits

### 1. Lint exclusion path pattern

```yaml
- path-except: "^ddtrace/tracer/"
  linters:
    - forbidigo
  text: "use github.com/DataDog/dd-trace-go/v2/internal/locking\\.(RW)?Mutex instead of sync\\.(RW)?Mutex"
```

This exclusion means the `sync.Mutex` lint rule only applies to `ddtrace/tracer/`. Files outside this directory can still use `sync.Mutex` freely. If the intent is to eventually migrate the entire codebase, consider expanding this or adding a TODO comment about the scope.

### 2. `format/go` Makefile target

The new `format/go` target is a nice convenience but the README update duplicates the target list that's already in the Makefile help output. This is minor documentation churn.

### 3. Removed migration checklist from `internal/locking/README.md`

The Phase 1/2/3 migration checklist is removed. Since this PR completes much of Phase 2 and Phase 3, the removal makes sense. However, consider adding a brief note about what has been completed and what remains (e.g., contrib packages still use `sync.Mutex`).

### 4. Comment on `finish()` call in `span.go`

The added comment is helpful:
```go
// Call context.finish() which handles trace-level bookkeeping and may modify
// this span (to set trace-level tags).
// Lock ordering is span.mu -> trace.mu.
```

Good documentation of the lock ordering invariant.
