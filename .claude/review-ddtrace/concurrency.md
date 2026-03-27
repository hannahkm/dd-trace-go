# Concurrency Reference

Concurrency bugs are the highest-severity class of review feedback in dd-trace-go. Reviewers catch data races, lock misuse, and unsafe shared state frequently. This file covers the patterns they flag.

## Mutex discipline

### Use checklocks annotations
This repo uses the `checklocks` static analyzer. When a struct field is guarded by a mutex, annotate it:

```go
type myStruct struct {
    mu sync.Mutex
    // +checklocks:mu
    data map[string]string
}
```

When you add a new field that's accessed under an existing lock, add the annotation. When you add a new method that accesses locked fields, the analyzer will verify correctness at compile time. Reviewers explicitly ask for `checklocks` and `checkatomic` annotations.

### Use assert.RWMutexLocked for helpers called under lock
When a helper function expects to be called with a lock already held, add a runtime assertion at the top:

```go
func (ps *prioritySampler) getRateLocked(spn *Span) float64 {
    assert.RWMutexLocked(&ps.mu)
    // ...
}
```

This documents the contract and catches violations at runtime. Import from `internal/locking/assert`.

### Don't acquire the same lock multiple times
A recurring review comment: "We're now getting the locking twice." If a function needs two values protected by the same lock, get both in one critical section:

```go
// Bad: two lock acquisitions
rate := ps.getRate(spn)       // locks ps.mu
loaded := ps.agentRatesLoaded // needs ps.mu again

// Good: one acquisition
ps.mu.RLock()
rate := ps.getRateLocked(spn)
loaded := ps.agentRatesLoaded
ps.mu.RUnlock()
```

### Don't invoke callbacks under a lock
Calling external code (callbacks, hooks, provider functions) while holding a mutex risks deadlocks if that code ever calls back into the locked structure. Capture what you need under the lock, release it, then invoke the callback:

```go
// Bad: callback under lock
mu.Lock()
cb := state.callback
if buffered != nil {
    cb(*buffered)  // dangerous: cb might call back into state
}
mu.Unlock()

// Good: release lock before calling
mu.Lock()
cb := state.callback
buffered := state.buffered
state.buffered = nil
mu.Unlock()

if buffered != nil {
    cb(*buffered)
}
```

This was flagged in multiple PRs (Remote Config subscription, OpenFeature forwarding callback).

## Atomic operations

### Prefer atomic.Value for write-once fields
When a field is set once from a goroutine and read concurrently, reviewers suggest `atomic.Value` over `sync.RWMutex` — it's simpler and sufficient:

```go
type Tracer struct {
    clusterID atomic.Value // stores string, written once
}

func (tr *Tracer) ClusterID() string {
    v, _ := tr.clusterID.Load().(string)
    return v
}
```

### Mark atomic fields with checkatomic
Similar to `checklocks`, use annotations for fields accessed atomically.

## Shared slice mutation

Appending to a shared slice is a race condition even if it looks safe:

```go
// Bug: r.config.spanOpts is shared across concurrent requests
// Appending can mutate the underlying array when it has spare capacity
options := append(r.config.spanOpts, tracer.ServiceName(serviceName))
```

This was flagged as P1 in a contrib PR. Always copy before appending:

```go
options := make([]tracer.StartSpanOption, len(r.config.spanOpts), len(r.config.spanOpts)+1)
copy(options, r.config.spanOpts)
options = append(options, tracer.ServiceName(serviceName))
```

## Global state

### Avoid adding global state
Reviewers push back on global variables, especially `sync.Once` guarding global booleans:

> "This is okay for now, however, this will be problematic when we try to parallelize the test runs. We should avoid adding global state like this if it is possible."

When you need process-level config, prefer passing it through struct fields or function parameters.

### Global state must reset on tracer restart
This repo supports `tracer.Start()` -> `tracer.Stop()` -> `tracer.Start()` cycles. Any global state that is set during `Start()` must be cleaned up or reset during `Stop()`, or the second `Start()` will operate on stale values.

**When reviewing code that uses global flags, `sync.Once`, or package-level variables, actively check:** does `Stop()` reset this state? If not, a restart cycle will silently reuse the old values. This was flagged on multiple PRs — for example, a `subscribed` flag that was set during `Start()` but never cleared in `Stop()`, causing the second `Start()` to skip re-subscribing because it thought the subscription was still active.

Common variants of this bug:
- A `sync.Once` guarding initialization: won't re-run after restart because `Once` is consumed
- A boolean flag like `initialized` or `subscribed`: if not reset in `Stop()`, the next `Start()` skips init
- A cached value (e.g., an env var read once): if the env var changed between stop and start, the stale value persists

Also: `sync.Once` consumes the once even on failure. If initialization can fail, subsequent calls return nil without retrying.

### Stale cached values that become outdated
Beyond the restart problem, reviewers question any value that is read once and cached indefinitely. When reviewing code that caches config, agent features, or other dynamic state, ask: "Can this change after initial load? If the agent configuration changes later, will this cached value become stale?"

Real examples:
- `telemetryConfig.AgentURL` loaded once from `c.agent` — but agent features are polled periodically and the URL could change
- A `sync.Once`-guarded `safe.directory` path computed from the first working directory — breaks if the process changes directories

### Map iteration order nondeterminism
Go map iteration order is randomized. When behavior depends on which key is visited first, results become nondeterministic. A P2 finding flagged this pattern: `setTags` iterates `StartSpanConfig.Tags` (a Go map), so when both `ext.ServiceName` and `ext.KeyServiceSource` are present, whichever key is visited last wins — making `_dd.svc_src` nondeterministic.

When code iterates a map and writes state based on specific keys, check whether the final state depends on iteration order. If it does, process the order-sensitive keys explicitly rather than relying on map iteration.

## Race-prone patterns in this repo

### Span field access during serialization
Spans are accessed concurrently (user goroutine sets tags, serialization goroutine reads them). All span field access after `Finish()` must go through the span's mutex. Watch for:
- Stats pipeline holding references to span maps (`s.meta`, `s.metrics`) that get cleared by pooling
- Benchmarks calling span methods without acquiring the lock

### Trace-level operations during partial flush
When the trace lock is released to acquire a span lock (lock ordering), recheck state after reacquiring the trace lock — another goroutine may have flushed or modified the trace in the interim.

### time.Time fields
`time.Time` is not safe for concurrent read/write. Fields like `lastFlushedAt` that are read from a worker goroutine and written from `Flush()` need synchronization.

## HTTP clients and shutdown

When a goroutine does HTTP polling (like `/info` discovery), use `http.NewRequestWithContext` tied to a cancellation signal so it doesn't block shutdown:

```go
// Bad: blocks shutdown until HTTP timeout
resp, err := httpClient.Get(url)

// Good: respects stop signal
req, _ := http.NewRequestWithContext(stopCtx, "GET", url, nil)
resp, err := httpClient.Do(req)
```

This was flagged because the polling goroutine is part of `t.wg`, and `Stop()` waits for the waitgroup — a slow/hanging HTTP request delays shutdown by the full timeout (10s default, 45s in CI visibility mode).
