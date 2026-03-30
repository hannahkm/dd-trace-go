# Code Review: PR #4495 -- feat(openfeature): subscribe to FFE_FLAGS during tracer RC setup

**PR**: https://github.com/DataDog/dd-trace-go/pull/4495
**Status**: MERGED
**Authors**: leoromanovsky, sameerank

---

## Blocking

### 1. Provider Shutdown does not detach callback from rcState -- stale callback persists after Shutdown()

**`internal/openfeature/rc_subscription.go`** (global `rcState`)
**`openfeature/remoteconfig.go:203`** (`stopRemoteConfig`)

When `DatadogProvider.Shutdown()` is called, `stopRemoteConfig()` only calls `remoteconfig.UnregisterCapability(FFEFlagEvaluation)`. It never resets `rcState.callback` back to nil or clears `rcState.subscribed`.

This means:

1. After `provider.Shutdown()`, the `forwardingCallback` still holds a reference to the now-shutdown provider's `rcCallback`. If the tracer's RC subscription continues delivering updates (the subscription itself is not removed), the forwarding callback will invoke `rcCallback` on a provider whose `configuration` has been set to nil and whose exposure writer is stopped. This may cause panics or silently corrupt state.

2. If a second `NewDatadogProvider()` is created after the first is shut down, `AttachCallback` at line 112 will see `rcState.callback != nil` (the stale callback from the first provider) and log a warning + return false, preventing the new provider from attaching. The second provider falls through to an error at `openfeature/remoteconfig.go:37`.

There needs to be a `DetachCallback()` or equivalent called from `stopRemoteConfig()` that clears `rcState.callback` (and optionally re-enables buffering).

### 2. Subscription token discarded in slow path -- Unsubscribe() is impossible

**`internal/openfeature/rc_subscription.go:150`**
**`openfeature/remoteconfig.go:199-201`**

In `SubscribeProvider`, the return value from `remoteconfig.Subscribe()` (the subscription token) is discarded with `_`. The comment at `openfeature/remoteconfig.go:199` acknowledges this. `stopRemoteConfig()` works around it by calling `UnregisterCapability`, but this only prevents the *capability* from being advertised; it does not actually remove the subscription callback from the RC client's internal list. The subscription callback remains registered and will continue to be invoked on RC updates. If a user calls `Shutdown()` and then creates a new provider, the old callback is still registered in the RC client, and a new `Subscribe` call for the same product will fail with a duplicate product error at `rc_subscription.go:146-147`.

The subscription token should be stored (e.g., in `rcState` or in the `DatadogProvider`) so that `stopRemoteConfig()` can call `remoteconfig.Unsubscribe(token)` for a clean teardown.

---

## Should Fix

### 3. `SubscribeRC` silently swallows errors from `HasProduct` when RC client is not started

**`internal/openfeature/rc_subscription.go:52,60`**

Both calls to `remoteconfig.HasProduct()` discard the error with `has, _ :=`. If the RC client has not been started yet (returns `ErrClientNotStarted`), `has` will be `false`, and the code proceeds to call `remoteconfig.Subscribe()` which may also fail. While the `Subscribe` error is handled, the silent discard masks a potential logic bug: the code cannot distinguish between "product not registered" and "client not started" -- two very different states requiring different handling.

At minimum, when `HasProduct` returns an error, the code should log it at debug level. Better: check for `ErrClientNotStarted` explicitly and handle accordingly.

### 4. `forwardingCallback` invokes provider callback under `rcState.Lock` -- risks blocking RC processing

**`internal/openfeature/rc_subscription.go:77-83`**

When `rcState.callback` is set, `forwardingCallback` calls it while holding `rcState.Lock()`. The `rcCallback` -> `updateConfiguration` path acquires `p.mu.Lock`. While there is no current deadlock risk (as the PR authors correctly noted in review comments), holding `rcState.Lock` during the entire provider callback execution means:

- Any other goroutine trying to call `AttachCallback`, `SubscribeRC`, or `SubscribeProvider` will block for the entire duration of the RC config processing (JSON unmarshal, validation, flag iteration).
- If the provider callback ever becomes slow (e.g., large config payloads), the RC processing thread is blocked.

A safer pattern would be to copy the callback reference under lock, release the lock, then invoke the callback. This was raised in review and dismissed, but the concern about blocking is valid even without deadlock risk.

### 5. `AttachCallback` replays buffered config under lock with same concern

**`internal/openfeature/rc_subscription.go:119-125`**

Same issue as above. The replay call `cb(rcState.buffered)` at line 124 runs under `rcState.Lock`. This blocks all other rcState operations during the entire replay, including any concurrent `forwardingCallback` invocations from the RC client. The buffered data and callback should be captured under lock, then the replay should happen after releasing the lock.

### 6. Exported test helpers ship in production binary

**`internal/openfeature/testing.go`**

`ResetForTest`, `SetSubscribedForTest`, `SetBufferedForTest`, and `GetBufferedForTest` are exported functions in a non-test file. They compile into the production binary and are callable by any internal consumer, allowing mutation of global state outside of tests.

These should be gated behind a build tag (e.g., `//go:build testutils` or `//go:build testing`) or placed in an `_test.go` file in the same package. The PR review discussion acknowledged this but dismissed it as infeasible; however, the `//go:build` approach is standard Go practice and straightforward.

### 7. `SubscribeProvider` slow-path error leaves RC client started but provider not subscribed

**`internal/openfeature/rc_subscription.go:141-155`**

In the slow path, if `remoteconfig.Start()` succeeds but `HasProduct` returns an unexpected result or `Subscribe` fails, the function returns an error but does not stop the RC client it just started. The caller (`startWithRemoteConfig`) propagates the error and returns a nil provider, but the RC client remains running in the background. There is no cleanup path for this case.

Consider calling `remoteconfig.Stop()` in the error paths after a successful `Start()`.

---

## Nits

### 8. `doc.go` still references hardcoded capability number 46

**`openfeature/doc.go:189`**

```
// the FFE_FLAGS product (capability 46). When new configurations are received,
```

Now that the capability is an iota constant (`remoteconfig.FFEFlagEvaluation`), this comment should reference the constant name rather than the magic number. If the iota block is ever reordered or a new constant is inserted before `FFEFlagEvaluation`, the doc will silently become wrong.

### 9. Copyright year is 2025 but files were created in 2026

**`internal/openfeature/rc_subscription.go:4`**
**`internal/openfeature/rc_subscription_test.go:4`**
**`internal/openfeature/testing.go:4`**
**`openfeature/rc_subscription.go:4`**
**`openfeature/rc_subscription_test.go:4`**

All new files have `Copyright 2025 Datadog, Inc.` but the commits are dated March 2026. This is presumably a minor oversight (or the repo template uses 2025). Low priority but worth noting for accuracy.

### 10. Inconsistent log formatting: `err.Error()` vs `%v` with `err`

**`ddtrace/tracer/remote_config.go:510`**

```go
log.Warn("openfeature: failed to subscribe to Remote Config: %v", err.Error())
```

Using `err.Error()` with `%v` is redundant -- `%v` on an error already calls `.Error()`. This was flagged and partially fixed in later commits but the instance in `remote_config.go:510` persists in the final diff. Should be `err` not `err.Error()`.

### 11. `SubscribeProvider` return type semantics are unintuitive

**`internal/openfeature/rc_subscription.go:133`**

The function returns `(tracerOwnsSubscription bool, err error)` where `true` means "the tracer already subscribed, you should use AttachCallback" and `false` means "we subscribed for you." The boolean name `tracerOwnsSubscription` is clear, but the caller in `openfeature/remoteconfig.go` then has to call `attachProvider` separately. This two-step dance (SubscribeProvider + attachProvider) is an API that's easy to misuse -- a caller could forget the second step. Consider consolidating the attach logic into `SubscribeProvider` or providing a single function that handles both paths.

### 12. `forwardingCallback` acknowledges configs it cannot validate

**`internal/openfeature/rc_subscription.go:92-96`**

When no provider callback is attached and the update is buffered, the function returns `ApplyStateAcknowledged` for all paths. This tells the RC infrastructure that the config was successfully applied, even though it has only been buffered and not validated. If the config turns out to be invalid when replayed later (during `AttachCallback`), the RC infrastructure will not be aware of the error. This is a known trade-off (documented in the code) but worth flagging as a correctness gap.
