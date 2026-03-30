# PR #4495 Review: feat(openfeature): subscribe to FFE_FLAGS during tracer RC setup

**PR Author:** leoromanovsky
**Status:** MERGED

## Summary

This PR moves the FFE_FLAGS Remote Config subscription into the tracer's `startRemoteConfig()` call, eliminating an extra RC poll interval (~5-8s) of latency when `NewDatadogProvider()` is called after `tracer.Start()`. It introduces a buffering/forwarding bridge in `internal/openfeature` that holds RC updates until the provider attaches, then replays them.

---

## Blocking

### 1. No cleanup of `rcState.callback` on provider Shutdown (fast path leak)

**File:** `internal/openfeature/rc_subscription.go:107-122` and `openfeature/provider.go:201-231`

When the provider shuts down via `Shutdown()` / `ShutdownWithContext()`, it calls `stopRemoteConfig()` which only calls `remoteconfig.UnregisterCapability(FFEFlagEvaluation)`. In the fast path (tracer owns the subscription), the `rcState.callback` still points to the now-dead provider's `rcCallback`. This means:

1. The `forwardingCallback` will continue forwarding RC updates to a shutdown provider, which sets `p.configuration` after `Shutdown()` already nil-ed it.
2. A subsequent `NewDatadogProvider()` call will fail with "callback already attached, multiple providers are not supported" because `rcState.callback != nil`.
3. No mechanism exists to detach/reset the callback -- there is no `DetachCallback()` function.

This is a lifecycle correctness bug that prevents provider re-creation and can cause writes to a shut-down provider.

### 2. `SubscribeProvider` return value / `AttachCallback` TOCTOU race

**File:** `internal/openfeature/rc_subscription.go:130-155` and `openfeature/remoteconfig.go:21-41`

`SubscribeProvider()` checks `rcState.subscribed` under the lock and returns `(true, nil)`. Then the caller **drops the lock** and calls `attachProvider()` -> `AttachCallback()`, which acquires the lock again. Between these two calls, a concurrent tracer restart could reset `rcState.subscribed = false` (via the re-subscription path in `SubscribeRC` lines 50-57), causing `AttachCallback` to return `false` even though `SubscribeProvider` just reported `true`. The comment on line 36 says "this shouldn't happen" but it can in the tracer-restart window.

This is an unlikely race in practice but represents a correctness gap in the serialization this code is explicitly designed to provide.

---

## Should Fix

### 3. `SubscribeRC` swallows the error from `HasProduct` when client is not started

**File:** `internal/openfeature/rc_subscription.go:52-53` and `63-64`

```go
if has, _ := remoteconfig.HasProduct(FFEProductName); has {
```

Both `HasProduct` calls discard the error. `HasProduct` returns `(false, ErrClientNotStarted)` when the client is nil. In the first check (line 52), if the RC client was destroyed during restart but the new one hasn't started yet, the error is silently ignored and the function falls through to `remoteconfig.Subscribe()` which will also fail with `ErrClientNotStarted`. The second check (line 63) has the same pattern. Consider at least logging the error, or distinguishing "not started" from "not found."

### 4. `log.Warn` format string uses `%v` with `err.Error()` -- double-stringification

**File:** `ddtrace/tracer/remote_config.go:510`

```go
log.Warn("openfeature: failed to subscribe to Remote Config: %v", err.Error())
```

`err.Error()` already returns a string. Using `%v` on a string is fine but the idiomatic pattern elsewhere in this codebase is `log.Warn("...: %v", err)` (passing the error directly). Using `.Error()` is redundant and inconsistent with the rest of the file. The same pattern appears at `internal/openfeature/rc_subscription.go:55`:

```go
log.Debug("openfeature: RC subscription for %s was lost (tracer restart?), re-subscribing", FFEProductName)
```
(This one is fine, just noting for contrast.)

### 5. `stopRemoteConfig` unregisters capability in both fast and slow paths, but only slow path registered it

**File:** `openfeature/remoteconfig.go:203-206`

In the fast path, the tracer registered the `FFEFlagEvaluation` capability via `SubscribeRC()` -> `remoteconfig.Subscribe(FFEProductName, ..., remoteconfig.FFEFlagEvaluation)`. When the provider shuts down and calls `stopRemoteConfig()` -> `UnregisterCapability(FFEFlagEvaluation)`, it removes a capability that was registered by the tracer's subscription. This could cause the tracer's FFE_FLAGS subscription to stop receiving updates even though the tracer itself hasn't stopped. The comment on lines 199-202 acknowledges this situation but the behavior is still incorrect for the fast path -- the provider should not unregister a capability it does not own.

### 6. Exported test helpers in non-test file have no build tag protection

**File:** `internal/openfeature/testing.go`

`ResetForTest`, `SetSubscribedForTest`, `SetBufferedForTest`, and `GetBufferedForTest` are exported functions in a non-`_test.go` file with no `//go:build` constraint. These functions mutate global state and will be included in production builds. While this is a pattern sometimes used in `internal` packages, it increases the binary size and attack surface unnecessarily. Consider either:
- Moving these to a `_test.go` file and having each test package set up state directly, or
- Adding a `//go:build testing` or `//go:build ignore` constraint, or
- Using `internal/openfeature/export_test.go` to re-export unexported helpers for tests in other packages.

### 7. Missing test for `SubscribeProvider` slow path error handling

**File:** `internal/openfeature/rc_subscription.go:137-155`

The slow path in `SubscribeProvider` calls `remoteconfig.Start()` and then `remoteconfig.Subscribe()`. There are no tests covering:
- The case where `remoteconfig.Start()` fails (line 140).
- The case where `HasProduct` returns true after Start but before Subscribe (line 144) -- meaning another subscriber raced in.
- The case where `Subscribe` fails (line 148).

Only the fast path (`tracerOwnsSubscription = true`) is tested in `TestStartWithRemoteConfigFastPath`.

---

## Nits

### 8. `FFEProductName` exported constant may be unnecessary

**File:** `internal/openfeature/rc_subscription.go:26`

`FFEProductName` is exported but only used within the `internal/openfeature` package and from `openfeature/doc.go` as documentation. Since this is in an `internal` package, exporting is fine for cross-package access within the module, but the constant is only used in `rc_subscription.go` itself. If no external consumer needs it, an unexported `ffeProductName` would be more conventional.

### 9. Inconsistent comment style on `ASMExtendedDataCollection`

**File:** `internal/remoteconfig/remoteconfig.go:134`

`ASMExtendedDataCollection` lacks a doc comment, unlike every other constant in the iota block. This is a pre-existing issue, not introduced by this PR, but the PR adds `FFEFlagEvaluation` directly after it with a proper comment, making the inconsistency more visible.

### 10. Test names do not follow Go test naming conventions

**File:** `internal/openfeature/rc_subscription_test.go` and `openfeature/rc_subscription_test.go`

Test names like `TestForwardingCallbackBuffersWhenNoCallback` and `TestAttachProviderReplaysBufferedConfig` are descriptive but quite long. This is a minor style point; the names are clear and serve their purpose.

### 11. `attachProvider` wrapper function is trivially thin

**File:** `openfeature/rc_subscription.go:16-17`

```go
func attachProvider(p *DatadogProvider) bool {
    return internalffe.AttachCallback(p.rcCallback)
}
```

This one-liner wrapper exists solely to adapt the provider to the internal package's `Callback` type. While it provides a named abstraction point, it adds an indirection layer that provides little value. If the intent is just to keep `internal/openfeature` free of provider-specific types, the call could be inlined at the single call site in `startWithRemoteConfig`.

### 12. `forwardingCallback` holds the lock while calling the provider callback

**File:** `internal/openfeature/rc_subscription.go:78-82`

```go
rcState.Lock()
defer rcState.Unlock()

if rcState.callback != nil {
    return rcState.callback(update)
}
```

The provider callback (`rcCallback` -> `processConfigUpdate`) acquires `DatadogProvider.mu` inside the `rcState.Lock()`. This creates a lock ordering dependency: `rcState.Mutex` -> `DatadogProvider.mu`. If any future code path acquires these in the opposite order, it will deadlock. This is not a bug today but is worth documenting as a lock-ordering invariant.
