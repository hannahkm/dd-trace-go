# Review: PR #4495 — feat(openfeature): subscribe to FFE_FLAGS during tracer RC setup

## Summary

This PR subscribes to the `FFE_FLAGS` Remote Config product during `tracer.startRemoteConfig()` so that feature flag configurations are included in the first RC poll. A forwarding callback in `internal/openfeature` buffers updates until the OpenFeature provider attaches, eliminating one full poll interval of latency. The hardcoded `ffeCapability = 46` is replaced with a named iota `FFEFlagEvaluation` in the capability block (value verified: still 46).

The architecture is clean: a thin internal bridge package with no OpenFeature SDK dependencies, a fast path (tracer subscribed) vs. slow path (provider starts RC itself), and proper serialization between the two subscription sources.

---

## Blocking

### 1. Callback invoked under lock in `AttachCallback` (`internal/openfeature/rc_subscription.go:124`)

`AttachCallback` calls `cb(rcState.buffered)` while holding `rcState.Lock()`. The `cb` here is `DatadogProvider.rcCallback`, which calls `processConfigUpdate` -> `provider.updateConfiguration`, which acquires the provider's own mutex. If the provider's code ever calls back into `rcState` (e.g., for status checks, or in future changes), this creates a deadlock risk. The concurrency guidance for this repo explicitly flags this pattern ("Don't invoke callbacks under a lock") and cites this exact PR family as an example.

Fix: capture `rcState.buffered` under the lock, nil it out, release the lock, then call `cb(buffered)` outside the critical section:

```go
rcState.callback = cb
buffered := rcState.buffered
rcState.buffered = nil
rcState.Unlock()

if buffered != nil {
    log.Debug("openfeature: replaying buffered RC config to provider")
    cb(buffered)
}
return true
```

This requires changing from `defer rcState.Unlock()` to manual unlock, but it eliminates the deadlock window.

### 2. Callback invoked under lock in `forwardingCallback` (`internal/openfeature/rc_subscription.go:81-82`)

Same pattern: `rcState.callback(update)` is called while holding `rcState.Lock()`. The RC client calls `forwardingCallback` from its poll loop, and the callback processes the update synchronously (JSON unmarshal, validation, provider state update). Holding the mutex for the entire duration of the provider callback blocks `AttachCallback`, `SubscribeRC`, and `SubscribeProvider` for the full processing time. More critically, if the provider callback ever needs to interact with `rcState` (directly or transitively), it deadlocks.

Fix: capture the callback reference under the lock, release the lock, then invoke:

```go
rcState.Lock()
cb := rcState.callback
rcState.Unlock()

if cb != nil {
    return cb(update)
}

// buffer path (re-acquire lock for buffering)
rcState.Lock()
defer rcState.Unlock()
// ... buffering logic ...
```

Note: this introduces a TOCTOU gap where the callback could be set between the check and the buffering. An alternative is to accept the lock-held invocation for the forwarding case (since the RC poll loop is single-threaded) but document the contract clearly. Either way, the current code should at minimum address the `AttachCallback` case (finding #1).

### 3. `SubscribeProvider` calls `remoteconfig.Start` and `remoteconfig.Subscribe` while holding `rcState.Lock()` (`internal/openfeature/rc_subscription.go:142-150`)

`remoteconfig.Start()` acquires `clientMux.Lock()` internally, and `remoteconfig.Subscribe()` acquires `client.productsMu.RLock()`. Holding `rcState.Lock()` while calling into `remoteconfig` functions that acquire their own locks creates a lock ordering dependency: `rcState.Mutex -> clientMux/productsMu`. Meanwhile, `SubscribeRC` (called from the tracer) also holds `rcState.Lock()` and calls `remoteconfig.HasProduct` and `remoteconfig.Subscribe`. If `SubscribeRC` and `SubscribeProvider` ever run concurrently, they both follow the same lock order (`rcState` first, then RC internals), so there is no immediate deadlock. However, `forwardingCallback` is called by the RC poll loop (which may hold RC-internal locks) and then acquires `rcState.Lock()` -- this is the reverse order (`RC internals -> rcState`), creating a potential deadlock cycle.

The safe fix is to check `rcState.subscribed` under the lock, release it, then do the RC operations without holding `rcState`:

```go
rcState.Lock()
if rcState.subscribed {
    rcState.Unlock()
    return true, nil
}
rcState.Unlock()

// RC operations without holding rcState.Lock()
if err := remoteconfig.Start(...); err != nil { ... }
if _, err := remoteconfig.Subscribe(...); err != nil { ... }
return false, nil
```

---

## Should fix

### 4. Test helpers exported in non-test production code (`internal/openfeature/testing.go`)

`ResetForTest`, `SetSubscribedForTest`, `SetBufferedForTest`, and `GetBufferedForTest` are exported functions in a non-test file that ships in production builds. The style guidance says "test helpers that mutate global state should be in `_test.go` files or build-tagged files, not shipped in production code."

These are only used from `_test.go` files in `internal/openfeature` and `openfeature`. Since they are cross-package test helpers (used by `openfeature/rc_subscription_test.go`), they cannot go in a `_test.go` file within `internal/openfeature`. The correct approach for this repo is to use an `export_test.go` file pattern or a build-tagged file (e.g., `//go:build testing`). Alternatively, consider whether the `openfeature` package tests could use a different test setup that doesn't need to reach into internal state.

### 5. `log.Warn` uses `%v` with `err.Error()` -- redundant `.Error()` call (`ddtrace/tracer/remote_config.go:510`)

```go
log.Warn("openfeature: failed to subscribe to Remote Config: %v", err.Error())
```

When using `%v` with an error, Go already calls `.Error()` implicitly. Passing `err.Error()` is redundant. The surrounding code in this file uses `%s` with `.Error()` (see `tracer.go:279`), or `%v` with `err` directly. Either `%v, err` or `%s, err.Error()` is fine, but `%v, err.Error()` is the inconsistent form.

### 6. Happy path not fully left-aligned in `startWithRemoteConfig` (`openfeature/remoteconfig.go:31-41`)

The function has the pattern:
```go
if !tracerOwnsSubscription {
    log.Debug(...)
    return provider, nil
}
if !attachProvider(provider) {
    return nil, fmt.Errorf(...)
}
log.Debug(...)
return provider, nil
```

This is actually reasonable since both branches return, but the early-return for `!tracerOwnsSubscription` means the "tracer owns" path is left-aligned, which is the correct orientation. No action strictly needed, but the comment `// This shouldn't happen since SubscribeProvider just told us tracer subscribed.` suggests this is defensive code for an impossible state -- consider whether this should be a `log.Error` + continue rather than returning a hard error that prevents provider creation.

### 7. Missing `checklocks` annotations on `rcState` fields (`internal/openfeature/rc_subscription.go:35-39`)

The `rcState` struct has fields guarded by `sync.Mutex` but no `checklocks` annotations. This repo uses the `checklocks` static analyzer to verify lock discipline at compile time. Add annotations:

```go
var rcState struct {
    sync.Mutex
    // +checklocks:Mutex
    subscribed bool
    // +checklocks:Mutex
    callback   Callback
    // +checklocks:Mutex
    buffered   remoteconfig.ProductUpdate
}
```

---

## Nits

### 8. Import grouping in `internal/openfeature/rc_subscription.go`

The imports mix Datadog agent (`github.com/DataDog/datadog-agent/...`) and Datadog tracer (`github.com/DataDog/dd-trace-go/...`) in the same group. The repo convention is three groups: stdlib, third-party, Datadog. The agent package is technically a separate org package but is conventionally grouped with Datadog imports. This is borderline and matches patterns elsewhere in the repo, so it may be fine -- just noting it for consistency review.

### 9. `FFEProductName` constant placement (`internal/openfeature/rc_subscription.go:25-27`)

The constant block wrapping a single constant with `const ( ... )` is slightly over-formal. A plain `const FFEProductName = "FFE_FLAGS"` would be simpler. Minor style point.

### 10. `SubscribeProvider` return value name `tracerOwnsSubscription` could be clearer

The returned bool means "did the tracer already subscribe (fast path)?" but the name `tracerOwnsSubscription` could be read as "does the tracer own the subscription going forward?" which is subtly different. Consider `tracerAlreadySubscribed` to match the semantic of "you can attach to the tracer's existing subscription."

---

## What looks good

- The `bytes.Clone` deep copy in `forwardingCallback` correctly prevents corruption if RC reuses byte buffers.
- The capability iota value (46) matches the old hardcoded constant exactly.
- The env var gating with `DD_EXPERIMENTAL_FLAGGING_PROVIDER_ENABLED` uses `internal.BoolEnv` which goes through the proper `internal/env` channel.
- The test coverage is solid: buffering, forwarding, replay, deep copy isolation, and tracer restart scenarios are all covered.
- The package boundary design (thin internal bridge with no OpenFeature SDK dependency) is well-considered.
- The `SubscribeRC` tracer-restart detection (checking `HasProduct` when `subscribed` is true) handles the `remoteconfig.Stop()` teardown case correctly.
