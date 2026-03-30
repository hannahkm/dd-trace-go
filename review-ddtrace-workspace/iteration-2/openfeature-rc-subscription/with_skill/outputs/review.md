# Review: PR #4495 — feat(openfeature): subscribe to FFE_FLAGS during tracer RC setup

## Summary

This PR subscribes to the `FFE_FLAGS` Remote Config product during `tracer.startRemoteConfig()` so feature flag configurations arrive on the first RC poll. A forwarding callback in `internal/openfeature` buffers the latest config until `NewDatadogProvider()` attaches, eliminating one full poll interval of latency (~5-8s). If the tracer did not subscribe (standalone provider), the provider falls back to its own RC subscription.

The overall design is sound and the test coverage is thorough. The findings below are primarily around concurrency safety (callback invoked under lock) and a few style/convention items.

---

## Blocking

### 1. Callback invoked under lock in `AttachCallback` — risk of deadlock

`internal/openfeature/rc_subscription.go:117-120` — `AttachCallback` calls `cb(rcState.buffered)` while holding `rcState.Mutex`. The `cb` is `DatadogProvider.rcCallback`, which calls `processConfigUpdate` -> `provider.updateConfiguration`, which acquires the provider's own mutex. If any code path in the provider ever calls back into `rcState` (e.g., via `AttachCallback` or `SubscribeProvider`), this creates a lock-ordering inversion. Even without a current deadlock, this violates the repo's concurrency convention: capture what you need under the lock, release it, then invoke the callback.

```go
// Current (dangerous):
rcState.Lock()
// ...
cb(rcState.buffered)        // callback under lock
rcState.buffered = nil
rcState.Unlock()

// Recommended:
rcState.Lock()
cb := cb                    // already have it
buffered := rcState.buffered
rcState.buffered = nil
rcState.callback = cb
rcState.Unlock()

if buffered != nil {
    cb(buffered)            // callback outside lock
}
```

This is the exact pattern called out in the concurrency reference for this repo, and was specifically flagged on an earlier iteration of this PR's own code (the forwarding callback).

### 2. `forwardingCallback` also invokes callback under lock

`internal/openfeature/rc_subscription.go:78-81` — Similarly, when `rcState.callback != nil`, the forwarding callback calls `rcState.callback(update)` while holding `rcState.Mutex`. The RC polling goroutine calls this callback, and the callback acquires the provider's mutex. Same lock-ordering concern as above.

```go
// Current:
rcState.Lock()
defer rcState.Unlock()
if rcState.callback != nil {
    return rcState.callback(update)  // callback under lock
}
```

Capture the callback reference and release the lock before invoking it.

### 3. `FFEFlagEvaluation` capability value must match the Remote Config specification

`internal/remoteconfig/remoteconfig.go:138-139` — `FFEFlagEvaluation` is appended to the iota block and resolves to value **46**, which matches the previously hardcoded `ffeCapability = 46`. However, the iota block's comment links to the [dd-source capabilities spec](https://github.com/DataDog/dd-source/blob/9b29208565b6e9c9644d8488520a24eb252ca1cb/domains/remote-config/shared/libs/rc/capabilities.go#L28). Confirm that value 46 is the canonical value for FFE flag evaluation in dd-source. If the spec assigns a different value (or if 46 is already taken by a different capability), this will silently break RC routing. The previous hardcoded `46` was correct by definition; moving to iota only stays correct if the iota ordering exactly mirrors the spec and no intermediate values were skipped or reordered in dd-source.

---

## Should fix

### 4. Global `rcState` is not reset on tracer Stop — stale state across restart cycles

`internal/openfeature/rc_subscription.go:36-40` — The `rcState` global (`subscribed`, `callback`, `buffered`) is never reset when `remoteconfig.Stop()` is called during `tracer.Stop()`. The `SubscribeRC` function does check whether the subscription was lost via `HasProduct`, but `rcState.callback` (the provider's callback reference) is never cleared. After a `tracer.Stop()` -> `tracer.Start()` cycle, the stale callback from the old provider remains attached, and the new provider will fail to attach because `AttachCallback` rejects a second callback ("callback already attached, multiple providers are not supported").

The concurrency reference specifically calls out that global state set during `Start()` must be cleaned up in `Stop()`. Consider either:
- Adding a `Reset()` call from the tracer's `Stop()` path (similar to `remoteconfig.Reset()`), or
- Clearing `rcState.callback` in `SubscribeRC` when it detects a lost subscription and re-subscribes.

The test `TestSubscribeRCAfterTracerRestart` partially covers this but does not exercise the full cycle with a provider attached, then stopped, then a new provider attaching.

### 5. `log.Warn` with `err.Error()` is redundant — use `%v` with `err` directly

`ddtrace/tracer/remote_config.go:510`:
```go
log.Warn("openfeature: failed to subscribe to Remote Config: %v", err.Error())
```
`%v` on an error already calls `.Error()`. Passing `err.Error()` formats the error as a string, which loses the `%w` wrapping if any downstream code unwraps. Use `err` directly:
```go
log.Warn("openfeature: failed to subscribe to Remote Config: %v", err)
```

### 6. Test helpers exported in production code

`internal/openfeature/testing.go` — `ResetForTest`, `SetSubscribedForTest`, `SetBufferedForTest`, and `GetBufferedForTest` are exported functions in non-test code that ships in production binaries. The style guide notes that test helpers mutating global state should be in `_test.go` files or build-tagged files. Consider either:
- Moving these to an `export_test.go` file in the same package (the standard Go pattern for exposing internals to external tests), or
- Adding a `//go:build testing` constraint.

### 7. `SubscribeProvider` discards the subscription token

`internal/openfeature/rc_subscription.go:141`:
```go
if _, err := remoteconfig.Subscribe(FFEProductName, cb, remoteconfig.FFEFlagEvaluation); err != nil {
```
The subscription token is discarded with `_`. The `stopRemoteConfig` comment in `openfeature/remoteconfig.go:199-202` acknowledges this and falls back to `UnregisterCapability`. However, losing the token means the subscription cannot be cleanly unsubscribed — `UnregisterCapability` only removes the capability bit but does not unregister the callback from the subscription list. If this is intentional, document why the token is not stored (e.g., "the subscription lifetime matches the RC client lifetime, which is managed by the tracer").

### 8. Happy path alignment in `startWithRemoteConfig`

`openfeature/remoteconfig.go:31-40`:
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
This is already mostly left-aligned, but the two return paths for `tracerOwnsSubscription == true` (success and the "shouldn't happen" error) could be slightly clearer. The `!tracerOwnsSubscription` early return is good. Minor nit, not blocking.

---

## Nits

### 9. Import alias `internalffe` is used inconsistently

`ddtrace/tracer/remote_config.go` and `openfeature/remoteconfig.go` both alias `internal/openfeature` as `internalffe`. The `ffe` abbreviation is not immediately obvious (FFE = Feature Flag Evaluation). A more descriptive alias like `internalof` or `internalOpenFeature` would improve readability, though this is a matter of taste.

### 10. `SubscribeRC` swallows the error from `HasProduct`

`internal/openfeature/rc_subscription.go:55-56`:
```go
if has, _ := remoteconfig.HasProduct(FFEProductName); has {
```
The error from `HasProduct` (which returns `ErrClientNotStarted` if the client is nil) is discarded. If the client is not started, `has` is `false` and the function proceeds to call `Subscribe`, which will also fail with `ErrClientNotStarted` — so the behavior is correct, but discarding the error without a comment makes the intent unclear.

### 11. `FFEProductName` constant placement

`internal/openfeature/rc_subscription.go:27` defines `FFEProductName = "FFE_FLAGS"`. Since this is a Remote Config product name, it might be more discoverable alongside the other product name constants (which are defined in `github.com/DataDog/datadog-agent/pkg/remoteconfig/state` as `state.ProductAPMTracing`, etc.). If adding to the agent repo is not feasible, the current location is acceptable.

### 12. Missing `ASMExtendedDataCollection` comment

`internal/remoteconfig/remoteconfig.go:134` — `ASMExtendedDataCollection` is missing a godoc comment (all other entries in the iota block have one). This is a pre-existing issue not introduced by this PR, but since the PR adds `FFEFlagEvaluation` right after it, it is worth noting.
