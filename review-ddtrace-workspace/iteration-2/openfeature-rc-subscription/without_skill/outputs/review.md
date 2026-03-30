# Code Review: PR #4495 - feat(openfeature): subscribe to FFE_FLAGS during tracer RC setup

## Summary

This PR adds early subscription to the `FFE_FLAGS` Remote Config product during `tracer.startRemoteConfig()`, eliminating a full RC poll interval (~5-8s) of latency when `NewDatadogProvider()` is called after `tracer.Start()`. It introduces `internal/openfeature` as a lightweight bridge between the tracer's early RC subscription and the late-created OpenFeature provider, using a forwarding/buffering pattern.

---

## Blocking

### 1. TOCTOU race between `SubscribeProvider` and `AttachCallback`

**File:** `openfeature/remoteconfig.go:26-38`
**File:** `internal/openfeature/rc_subscription.go:131-156`

In `startWithRemoteConfig`, `SubscribeProvider()` checks `rcState.subscribed` under the lock and returns `true`, then drops the lock. After the lock is released, `attachProvider()` calls `AttachCallback()` which re-acquires the lock and checks `rcState.subscribed` again. Between these two calls, a concurrent `SubscribeRC()` from a tracer restart could alter `rcState.subscribed` (setting it to `false` and then `true` with a new subscription), or another provider could call `AttachCallback` and set `rcState.callback` first, causing the second `AttachCallback` to return `false` ("callback already attached").

The result is that `SubscribeProvider` returns `tracerOwnsSubscription=true`, but then `attachProvider` returns `false`, causing a hard error ("failed to attach to tracer's RC subscription") even though the comment says "This shouldn't happen." Under tracer restart timing or multiple `NewDatadogProvider` calls, it can happen.

**Suggestion:** Either perform both the subscription check and the callback attachment atomically in a single function call that holds the lock throughout, or have `SubscribeProvider` in the fast path also set the callback (accepting the callback as a parameter) so there is no gap.

### 2. Provider shutdown does not detach the callback from `rcState`

**File:** `openfeature/remoteconfig.go:203-207`
**File:** `internal/openfeature/rc_subscription.go:104-128`

When a provider shuts down via `stopRemoteConfig()`, it only calls `remoteconfig.UnregisterCapability`. It does not clear `rcState.callback`. This means:
- The `forwardingCallback` will continue forwarding RC updates to the now-dead provider's `rcCallback`, which writes to a provider whose `configuration` has been set to `nil`.
- If a user creates a new provider after shutting down the old one, `AttachCallback` will fail with "callback already attached, multiple providers are not supported" because the old callback is still registered.

**Suggestion:** Add a `DetachCallback()` function to `internal/openfeature` that clears `rcState.callback` (and optionally re-enables buffering), and call it from `stopRemoteConfig()`.

---

## Should Fix

### 3. `SubscribeProvider` slow path discards the subscription token

**File:** `internal/openfeature/rc_subscription.go:148-149`

In the slow path, `remoteconfig.Subscribe(FFEProductName, cb, remoteconfig.FFEFlagEvaluation)` returns a `SubscriptionToken` which is assigned to `_` (discarded). The comment in `stopRemoteConfig` acknowledges this:

> "In the slow path, this package discards the subscription token from Subscribe(), so we cannot call Unsubscribe()."

This means there is no way to properly unsubscribe in the slow path. `UnregisterCapability` stops receiving updates but the subscription remains registered in the RC client, preventing re-subscription (the RC client's `Subscribe` will return "product already registered" if the same product is subscribed again). If the user creates a provider, shuts it down, then creates another, the second `Subscribe` call in `SubscribeProvider` may fail because `HasProduct` returns `true` from the orphaned subscription.

**Suggestion:** Store the `SubscriptionToken` (perhaps in `rcState`) and call `remoteconfig.Unsubscribe` on shutdown instead of relying on `UnregisterCapability`.

### 4. `forwardingCallback` holds the mutex while calling the provider callback

**File:** `internal/openfeature/rc_subscription.go:77-97`

`forwardingCallback` acquires `rcState.Lock()` and, if `rcState.callback != nil`, calls it while still holding the lock. If the callback (`DatadogProvider.rcCallback`) takes a non-trivial amount of time (e.g., parsing JSON, validating configs), this blocks all other operations on `rcState` for the duration: `AttachCallback`, `SubscribeRC`, `SubscribeProvider`, and all test helper functions.

More critically, if the callback ever tries to call back into `internal/openfeature` (e.g., to check state), it will deadlock because `sync.Mutex` is not reentrant.

**Suggestion:** Copy the callback reference under the lock, release the lock, then invoke the callback. This is the standard Go pattern for callback invocation under a mutex:

```go
rcState.Lock()
cb := rcState.callback
rcState.Unlock()
if cb != nil {
    return cb(update)
}
// ... buffering path ...
```

### 5. `SubscribeRC` ignores the error from `HasProduct` when the RC client is not started

**File:** `internal/openfeature/rc_subscription.go:49-60`

`HasProduct` returns `(bool, error)` and returns `ErrClientNotStarted` when the client is nil. In `SubscribeRC`, this error is silently discarded with `has, _ := ...`. If the RC client has not been started yet when `SubscribeRC` is called, `HasProduct` will return `(false, ErrClientNotStarted)`, and the code will fall through to `remoteconfig.Subscribe`, which will also fail with `ErrClientNotStarted`. The `Subscribe` error is handled, but the intent of the `HasProduct` check (to detect an existing subscription) is defeated when the client is not started.

Additionally, on line 62, the second `HasProduct` call also discards the error.

**Suggestion:** At minimum, if `HasProduct` returns an error that is not `ErrClientNotStarted`, propagate it. The `ErrClientNotStarted` case should not reach `HasProduct` in normal flow (since this is called from `startRemoteConfig` after the RC client is started), but defensive error handling would be prudent.

### 6. Capability value is now coupled to iota ordering -- fragile for a wire protocol value

**File:** `internal/remoteconfig/remoteconfig.go:138-139`

The old code hardcoded `ffeCapability = 46`, which was an explicit wire-protocol value matching the Remote Config specification. The PR replaces this with an `iota` entry. Since `Capability` values are bit indices sent over the wire to the agent, their numeric values are part of the protocol contract. Adding `FFEFlagEvaluation` at the end of the iota block gives it value 46 today, which is correct. However, if anyone inserts a new capability above it in the iota list, `FFEFlagEvaluation` silently changes value and breaks the wire protocol.

The existing capabilities have this same fragility, so this is consistent with the codebase convention. But the PR description mentions the move from hardcoded 46 to iota as a positive change, and it warrants a note that the ordering in this iota block is load-bearing and must never be reordered.

**Suggestion:** Add a comment near the `const` block (or near `FFEFlagEvaluation`) stating that these iota values are wire-protocol indices and must not be reordered. Alternatively, add a compile-time assertion like `var _ [46]struct{} = [FFEFlagEvaluation]struct{}{}` to catch accidental shifts.

---

## Nits

### 7. `log.Warn` should use `%v`, not `err.Error()`

**File:** `ddtrace/tracer/remote_config.go:510`

```go
log.Warn("openfeature: failed to subscribe to Remote Config: %v", err.Error())
```

The `%v` verb already calls `.Error()` on error values. Calling `err.Error()` explicitly means the format string receives a `string`, not an `error`. This is fine functionally but is inconsistent with the rest of the codebase which passes `err` directly. Using `err` is also more idiomatic.

**Suggestion:** `log.Warn("openfeature: failed to subscribe to Remote Config: %v", err)`

### 8. `testing.go` exports test helpers in a non-test file

**File:** `internal/openfeature/testing.go`

`ResetForTest`, `SetSubscribedForTest`, `SetBufferedForTest`, and `GetBufferedForTest` are exported functions in a non-test file. This means they are available to any production code that imports `internal/openfeature`, not just tests. While the `internal/` path restricts external access, any code within `dd-trace-go` can call `ResetForTest()` in production.

The standard Go convention for test-only helpers is to put them in a `_test.go` file (which is only compiled during `go test`). If these helpers need to be used from tests in a different package (e.g., `openfeature/rc_subscription_test.go`), the typical pattern is to use an `export_test.go` file in the same package that re-exports internal state for testing.

**Suggestion:** Consider using `export_test.go` or at minimum adding a clear doc comment like `// ResetForTest is for testing only. Do not call from production code.` (which is partially done but could be more emphatic).

### 9. Missing test for `SubscribeProvider` slow path

**File:** `openfeature/rc_subscription_test.go`

The test suite covers the fast path (`TestStartWithRemoteConfigFastPath`) but there is no integration test that exercises the slow path of `SubscribeProvider` where the tracer has not subscribed and the provider must call `remoteconfig.Start` + `remoteconfig.Subscribe` itself. This is a significant code path that is now different from the original implementation.

### 10. `SubscribeProvider` does not set `rcState.subscribed` in the slow path

**File:** `internal/openfeature/rc_subscription.go:131-156`

When `SubscribeProvider` takes the slow path (tracer did not subscribe), it calls `remoteconfig.Start` and `remoteconfig.Subscribe` but does not set `rcState.subscribed = true`. This means if `SubscribeRC` is called later (e.g., a late tracer start), it will try to subscribe to `FFE_FLAGS` again, hitting the "already subscribed" check in `HasProduct`. The `HasProduct` guard on line 62 of `SubscribeRC` should catch this and skip, so it is not a crash, but the state is inconsistent: the product is subscribed but `rcState.subscribed` is `false`.

### 11. Minor: unused import potential

**File:** `openfeature/remoteconfig.go:12`

The `maps` import is present and used in `validateConfiguration`. This is not changed by the PR, just noting it is retained correctly.

### 12. Product name constant duplication avoidance

**File:** `internal/openfeature/rc_subscription.go:25-27`

Good decision to define `FFEProductName = "FFE_FLAGS"` as a constant and use it throughout. This eliminates the string duplication that existed before.
