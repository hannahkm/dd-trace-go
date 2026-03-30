# PR #4523: fix(tracer): only set _dd.p.ksr after agent rates are received

## Summary

This PR fixes `_dd.p.ksr` (Knuth Sampling Rate) propagation so it is only set on spans after the agent has actually provided sampling rates via `readRatesJSON()`. Previously, `_dd.p.ksr` was unconditionally set in `prioritySampler.apply()`, including when the rate was the initial client-side default (1.0) before any agent response arrived. This aligns Go with the behavior of Python, Java, PHP, and other tracers.

The PR also refactors `prioritySampler` to consolidate lock acquisitions by extracting `getRateLocked()` so that `apply()` acquires `ps.mu.RLock` only once to read both the rate and `agentRatesLoaded`.

**Files changed:** `ddtrace/tracer/sampler.go`, `ddtrace/tracer/sampler_test.go`

---

## Blocking

None identified.

---

## Should Fix

### 1. `getRateLocked` assert annotation may not match build-tag gating

`getRateLocked` uses `assert.RWMutexRLocked(&ps.mu)` at runtime, but the `+checklocksignore` annotation tells the static checker to skip this method. Since `ps.mu` is a `locking.RWMutex` (not `sync.RWMutex`), the runtime assertion only fires under the `deadlock` build tag. This is fine for dynamic analysis, but the `+checklocksignore` annotation on `getRateLocked` means the static `checklocks` tool will never verify that callers hold the lock. Consider using `+checklocksfunc:ps.mu` (or the equivalent positive annotation) instead of `+checklocksignore` so that the static analyzer enforces the invariant at compile time. The `checklocksignore` comment rationale ("Called during initialization in StartSpan, span not yet shared") is copied from `getRate` but no longer applies to `getRateLocked` itself, which is a general-purpose locked helper.

**File:** `ddtrace/tracer/sampler.go`, `getRateLocked` function

### 2. `agentRatesLoaded` is never reset

Once `agentRatesLoaded` is set to `true` in `readRatesJSON`, it is never reset. If the agent connection is lost and the priority sampler falls back to default rates, `_dd.p.ksr` will still be set (because `agentRatesLoaded` remains `true`). This may be the intended behavior (once rates arrive, they are considered "real"), but it is worth confirming this matches the cross-language RFC specification. If the intent is that ksr should only be set while actively receiving agent rates, a mechanism to reset the flag on timeout or empty rate responses would be needed.

---

## Nits

### 1. Minor: lock scope in `apply` could use defer

In `apply()`, the lock is manually acquired and released:
```go
ps.mu.RLock()
rate := ps.getRateLocked(spn)
fromAgent := ps.agentRatesLoaded
ps.mu.RUnlock()
```

Using `defer` would be more idiomatic and safer against future modifications that might add early returns:
```go
ps.mu.RLock()
defer ps.mu.RUnlock()
rate := ps.getRateLocked(spn)
fromAgent := ps.agentRatesLoaded
```

However, the current form is fine since the critical section is intentionally narrow and the subsequent code does not need the lock. This is a style preference.

### 2. Comment accuracy on `getRateLocked`

The `+checklocksignore` comment says "Called during initialization in StartSpan, span not yet shared." This was accurate for `getRate` (where the span-level fields are accessed without the span lock), but `getRateLocked` is about the *prioritySampler* lock, not the span lock. The comment should be updated to reflect the actual invariant (caller holds `ps.mu`).
