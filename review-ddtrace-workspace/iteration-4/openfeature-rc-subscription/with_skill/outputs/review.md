# Review: PR #4495 — feat(openfeature): subscribe to FFE_FLAGS during tracer RC setup

## Summary

This PR subscribes to the `FFE_FLAGS` Remote Config product during `tracer.startRemoteConfig()` so the first RC poll includes feature flag data. A new `internal/openfeature` package bridges the timing gap between the tracer's early RC subscription and the late-created `DatadogProvider`. When the provider is created, it either replays buffered config (fast path) or falls back to its own RC subscription (slow path). The PR also moves the hardcoded `ffeCapability = 46` into the remoteconfig capability iota as `FFEFlagEvaluation`.

## Reference files consulted

- style-and-idioms.md (always)
- concurrency.md (mutex, global state, callback-under-lock patterns)

## Blocking

### 1. Callback invoked under lock in `AttachCallback` -- potential deadlock

`internal/openfeature/rc_subscription.go:119-125`

`AttachCallback` calls `cb(rcState.buffered)` while holding `rcState.Lock()`. The callback is `DatadogProvider.rcCallback`, which calls `processConfigUpdate`, which calls `provider.updateConfiguration`, which acquires `p.mu.Lock()`. If any code path ever acquires `p.mu` first and then calls into `rcState` (or if RC invokes `forwardingCallback` concurrently), this creates a lock-ordering risk. The concurrency guide explicitly flags this pattern: "Don't invoke callbacks under a lock... Capture what you need under the lock, release it, then invoke the callback."

The same issue exists in `forwardingCallback` at line 81-83, where `rcState.callback(update)` is called under `rcState.Lock()`. This means every RC update that arrives after the provider is attached will invoke the full `rcCallback` -> `updateConfiguration` -> `p.mu.Lock()` chain while holding `rcState.Mutex`. This is worse than the replay case because it happens on every update, not just once.

**Fix:** Capture the callback and buffered data under the lock, release it, then invoke the callback outside. For `forwardingCallback`, capture the callback reference under the lock and call it after `Unlock()`.

### 2. Global `rcState.subscribed` is never reset on tracer `Stop()`

`internal/openfeature/rc_subscription.go:35-39`

The concurrency guide calls out this exact bug pattern: "When reviewing code that uses global flags, `sync.Once`, or package-level variables, actively check: does `Stop()` reset this state?" The `rcState.subscribed` flag is set to `true` during `SubscribeRC()` but is only reset inside `SubscribeRC()` itself (when it detects the subscription was lost). The tracer's `Stop()` method at `ddtrace/tracer/tracer.go:977` calls `remoteconfig.Stop()` which destroys the RC client and all subscriptions, but never resets `rcState`.

The `SubscribeRC` function does try to handle this by checking `remoteconfig.HasProduct()`, which should return false after a restart. However, `rcState.callback` is never cleared -- so after a stop/start cycle, the old provider's callback remains wired in. If a new provider is created, `AttachCallback` at line 112 will log a warning and return `false`, breaking the fast path silently.

**Fix:** Add an exported `Reset()` function (not just `ResetForTest`) that the tracer's `Stop()` calls, or have `SubscribeRC` also clear `rcState.callback` when it detects a lost subscription (it currently only clears `callback` on line 57, but only when `subscribed` is true AND `HasProduct` returns false -- if `HasProduct` returns false because of a race with Stop, the callback is cleared, but if it returns an error, it is not).

### 3. `internal.BoolEnv` used instead of `internal/env` for config check

`ddtrace/tracer/remote_config.go:508`

The style guide explicitly states: "Environment variables must go through `internal/env` (or `instrumentation/env` for contrib), never raw `os.Getenv`. Note: `internal.BoolEnv` and similar helpers in the top-level `internal` package are **not** the same as `internal/env` -- they are raw `os.Getenv` wrappers that bypass the validated config pipeline." The existing `NewDatadogProvider` in `openfeature/provider.go:76` also uses `internal.BoolEnv`, so this is a pre-existing issue, but the new code in the tracer package should not replicate it. The env var `DD_EXPERIMENTAL_FLAGGING_PROVIDER_ENABLED` is already registered in `internal/env/supported_configurations.gen.go`, so it should be read through `internal/env`.

## Should fix

### 4. Magic string for env var instead of using the existing constant

`ddtrace/tracer/remote_config.go:508`

The string `"DD_EXPERIMENTAL_FLAGGING_PROVIDER_ENABLED"` is hardcoded here, but it already exists as the constant `ffeProductEnvVar` in `openfeature/provider.go:35`. While importing from `openfeature` into `ddtrace/tracer` might create a cycle, the constant could be defined in `internal/openfeature` (alongside `FFEProductName`) and imported by both packages. Duplicating the string risks them drifting apart.

### 5. Exported test helpers in non-test production code

`internal/openfeature/testing.go`

`ResetForTest`, `SetSubscribedForTest`, `SetBufferedForTest`, and `GetBufferedForTest` are exported functions in a non-test file that ships in production builds. The style guide says "Test helpers that mutate global state should be in `_test.go` files or build-tagged files, not shipped in production code." These should either live in a `_test.go` file (if only needed by tests in the same package) or be gated with a build tag. Since they are used from `openfeature/rc_subscription_test.go` (a different package), one approach is an `export_test.go` pattern or an `internal/openfeature/testutil` sub-package.

### 6. Error message does not describe impact

`ddtrace/tracer/remote_config.go:510`

The warning `"openfeature: failed to subscribe to Remote Config: %v"` describes what failed but not the user impact. Per the style guide, the message should explain what is lost, for example: `"openfeature: failed to subscribe to Remote Config; feature flag configs will not be pre-fetched and the provider will fall back to its own subscription: %v"`.

### 7. `err.Error()` with `%v` is redundant

`ddtrace/tracer/remote_config.go:510`

`log.Warn("openfeature: failed to subscribe to Remote Config: %v", err.Error())` -- using `%v` on `err.Error()` is redundant since `%v` on an `error` already calls `Error()`. Should be either `log.Warn("... %v", err)` or `log.Warn("... %s", err.Error())`. The same pattern appears in `openfeature/remoteconfig.go:73` and `:83` (pre-existing).

### 8. Happy path nesting in `startWithRemoteConfig`

`openfeature/remoteconfig.go:31-41`

The control flow nests the fast path inside two conditions. A clearer structure would use early returns:

```go
if tracerOwnsSubscription {
    if !attachProvider(provider) {
        return nil, fmt.Errorf("failed to attach to tracer's RC subscription")
    }
    log.Debug("openfeature: attached to tracer's RC subscription")
    return provider, nil
}
log.Debug("openfeature: successfully subscribed to Remote Config updates")
return provider, nil
```

This is minor since the function is short, but the current structure puts the "shouldn't happen" error case inside the happy path block.

## Nits

### 9. Import alias consistency

The alias `internalffe` is used in three files (`ddtrace/tracer/remote_config.go`, `openfeature/remoteconfig.go`, `openfeature/rc_subscription_test.go`). This is consistent, which is good. However, the alias name `ffe` is not immediately obvious -- a comment near the first import or a more descriptive alias like `internalof` (for openfeature) could improve readability.

### 10. `FFEProductName` could use a comment explaining the abbreviation

`internal/openfeature/rc_subscription.go:26`

The comment says "RC product name for feature flag evaluation" but doesn't mention that "FFE" stands for "Feature Flag Evaluation." A reader unfamiliar with the product name convention might not connect the abbreviation.

### 11. `SubscribeProvider` naming

`internal/openfeature/rc_subscription.go:133`

The function name `SubscribeProvider` suggests it subscribes the provider, but in the fast path it just returns `true` without doing any subscription work. The actual attachment happens later via `AttachCallback`. A name like `EnsureSubscription` or documenting the two-step protocol more prominently would reduce confusion.
