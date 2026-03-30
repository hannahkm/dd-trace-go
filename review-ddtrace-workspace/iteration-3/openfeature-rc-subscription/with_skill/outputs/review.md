# Review: PR #4495 — feat(openfeature): subscribe to FFE_FLAGS during tracer RC setup

## Summary

This PR subscribes to the `FFE_FLAGS` Remote Config product during `tracer.startRemoteConfig()` so that flag configurations arrive on the first RC poll, eliminating one full poll-interval of latency when `NewDatadogProvider()` is called after `tracer.Start()`. It introduces `internal/openfeature` as a lightweight bridge that buffers RC updates until the provider attaches, and refactors the provider's RC setup to use this shared subscription when available ("fast path") or fall back to its own subscription ("slow path").

The design is sound and well-motivated. The deep-copy of buffered payloads, serialization of tracer/provider subscription, and explicit rejection of multiple providers are all good correctness improvements. Below are the issues found against the loaded guidance.

---

## Blocking

### 1. Callback invoked under lock in `AttachCallback` (`internal/openfeature/rc_subscription.go:124`)

`AttachCallback` calls `cb(rcState.buffered)` at line 124 while holding `rcState.Lock()`. The callback is `DatadogProvider.rcCallback`, which calls `processConfigUpdate` -> `provider.updateConfiguration` -- if `updateConfiguration` ever acquires its own lock, or if a future change has the callback interact with anything that touches `rcState`, this deadlocks. The concurrency guidance explicitly flags this pattern: "Calling external code (callbacks, hooks, provider functions) while holding a mutex risks deadlocks if that code ever calls back into the locked structure."

The same issue exists in `forwardingCallback` at line 82, where `rcState.callback(update)` is called under `rcState.Lock()`.

**Fix:** Capture the callback and buffered data under the lock, release the lock, then invoke the callback outside the critical section.

### 2. `rcState` global is never reset on tracer `Stop()` (`internal/openfeature/rc_subscription.go:35`)

The concurrency guidance calls this out explicitly: "Any global state that is set during `Start()` must be cleaned up or reset during `Stop()`, or the second `Start()` will operate on stale values." The `rcState.subscribed` flag is set during `SubscribeRC()` (called from `tracer.startRemoteConfig`), but `tracer.Stop()` does not reset it.

While `SubscribeRC` does attempt to detect a lost subscription via `HasProduct`, this detection depends on the new RC client being started *before* `SubscribeRC` runs -- which is true in the current code path, but is fragile. More importantly, `rcState.callback` is never cleared on stop. If a provider attached a callback during the first tracer lifecycle, that stale callback persists into the second lifecycle and will receive updates meant for a new provider.

There should be a `Reset()` function (or similar) called from the tracer's `Stop()` path, analogous to `remoteconfig.Stop()` already being called there.

---

## Should fix

### 3. `internal.BoolEnv` used directly in `ddtrace/tracer/remote_config.go:508`

The universal checklist states: "Environment variables must go through `internal/env` (or `instrumentation/env` for contrib), never raw `os.Getenv`... `internal.BoolEnv` and similar helpers in the top-level `internal` package are **not** the same as `internal/env`." However, checking the actual implementation, `internal.BoolEnv` delegates to `env.Lookup` internally (via `BoolEnvNoDefault`), so this is not as severe as the guidance suggests -- the value does flow through `internal/env`. That said, the same env var `DD_EXPERIMENTAL_FLAGGING_PROVIDER_ENABLED` is read via `internal.BoolEnv` in `openfeature/provider.go:76` and `ddtrace/tracer/remote_config.go:508` without a shared constant. Consider defining the constant once (as `ffeProductEnvVar` already exists in `openfeature/provider.go:35`) and importing it, or using a shared constant in the `internal/openfeature` package.

### 4. Magic string `"DD_EXPERIMENTAL_FLAGGING_PROVIDER_ENABLED"` duplicated (`ddtrace/tracer/remote_config.go:508`)

The env var name appears as a raw string literal in the tracer file, while `openfeature/provider.go` already defines `ffeProductEnvVar = "DD_EXPERIMENTAL_FLAGGING_PROVIDER_ENABLED"`. The universal checklist flags magic strings that already have a named constant elsewhere. The tracer should reference the constant rather than duplicating the string.

### 5. Test helpers exported in production code (`internal/openfeature/testing.go`)

`ResetForTest`, `SetSubscribedForTest`, `SetBufferedForTest`, and `GetBufferedForTest` are exported functions in a non-test file that ships in production builds. The style guidance says: "Test helpers that mutate global state should be in `_test.go` files or build-tagged files, not shipped in production code." These functions allow arbitrary mutation of the global `rcState` from any importing package.

Consider either:
- Moving them to a `testing_test.go` file (if only used within the same package) -- though they are used cross-package.
- Adding a build tag like `//go:build testing` to gate them out of production builds.
- Using an `internal/openfeature/testutil` sub-package with a test build constraint.

### 6. `log.Warn` format passes `err.Error()` instead of `err` (`ddtrace/tracer/remote_config.go:510`)

```go
log.Warn("openfeature: failed to subscribe to Remote Config: %v", err.Error())
```

Passing `err.Error()` to `%v` is redundant -- `%v` on an `error` already calls `.Error()`. More importantly, if `err` is nil (which cannot happen here since we're inside the `err != nil` guard), calling `.Error()` on nil would panic. Using `err` directly is more idiomatic:

```go
log.Warn("openfeature: failed to subscribe to Remote Config: %v", err)
```

### 7. Error message lacks impact context (`ddtrace/tracer/remote_config.go:510`)

The universal checklist asks error messages to describe what the user loses. The current message "failed to subscribe to Remote Config" doesn't explain the consequence. A more helpful message would be something like: `"openfeature: failed to subscribe to Remote Config; feature flag configs will not be available until provider creates its own subscription: %v"`.

### 8. `SubscribeProvider` discards the subscription token (`internal/openfeature/rc_subscription.go:150`)

In the slow path, `remoteconfig.Subscribe` returns a token that is discarded (`_, err := remoteconfig.Subscribe(...)`). The `stopRemoteConfig` comment acknowledges this: "this package discards the subscription token from Subscribe(), so we cannot call Unsubscribe()." While this is documented, it means the subscription can never be properly cleaned up. If practical, consider storing the token so `stopRemoteConfig` can call `Unsubscribe()` instead of relying on `UnregisterCapability`.

---

## Nits

### 9. Import alias `internalffe` is somewhat opaque

The alias `internalffe` for `internal/openfeature` is used in both `ddtrace/tracer/remote_config.go` and `openfeature/remoteconfig.go`. Since the package is already named `openfeature`, the alias is needed to avoid collision -- but `internalffe` doesn't obviously map to "internal openfeature." Consider `internalof` or `intoff` for slightly better readability, though this is purely a preference.

### 10. `FFEProductName` could be unexported

`FFEProductName` is exported but only used within `internal/openfeature` and in tests. If it doesn't need to be visible outside the package, making it unexported (`ffeProductName`) would reduce API surface per the "don't add unused API surface" guidance.

### 11. `Callback` type could be unexported

Similarly, the `Callback` type at `internal/openfeature/rc_subscription.go:31` is exported but only referenced internally. Unless external consumers need to construct callbacks, consider `callback`.

### 12. Comment on `ASMExtendedDataCollection` missing (`internal/remoteconfig/remoteconfig.go:134`)

Not introduced by this PR, but `ASMExtendedDataCollection` (immediately above the new `APMTracingMulticonfig`) lacks a godoc comment while all other capabilities have one. Since this PR adds `FFEFlagEvaluation` with a proper comment right next to it, the inconsistency becomes more visible. Consider adding a comment to `ASMExtendedDataCollection` in the same change.
