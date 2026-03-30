# Code Review: PR #4550 - refactor(config): Migrate agentURL and traceProtocol

**PR**: https://github.com/DataDog/dd-trace-go/pull/4550
**Author**: mtoffl01
**Status**: Merged
**Base**: main

## Summary

This PR migrates the `agentURL`, `originalAgentURL`, and `traceProtocol` fields from the tracer-level `config` struct (`ddtrace/tracer/option.go`) into the centralized `internal/config.Config`. It replaces `internal.AgentURLFromEnv()` usage in the tracer with a new `resolveAgentURL()` helper in `internal/config/config_helpers.go` that reads env vars through the config provider (enabling telemetry). It also moves `DD_TRACE_AGENT_PROTOCOL_VERSION` resolution into `internal/config` and introduces a `RawAgentURL()`/`AgentURL()` split: `RawAgentURL()` returns the configured URL as-is, `AgentURL()` rewrites unix-scheme URLs to the `http://UDS_...` transport form.

---

## Blocking

### 1. Behavioral change to v1 protocol enablement -- unconditional downgrade on missing agent endpoint

**Files**: `ddtrace/tracer/option.go` (diff lines ~135-148), `ddtrace/tracer/option.go:776` (old line ~780)

**Old behavior**: `fetchAgentFeatures` only set `features.v1ProtocolAvailable = true` when the agent reported the `/v1.0/traces` endpoint AND `DD_TRACE_AGENT_PROTOCOL_VERSION` was `"1.0"`. Then in `newConfig`, if `af.v1ProtocolAvailable` was true, it upgraded `c.traceProtocol` to v1 and rewrote the transport URL.

**New behavior (in the PR diff)**: `fetchAgentFeatures` now unconditionally sets `features.v1ProtocolAvailable = true` whenever the agent reports `/v1.0/traces` (the env-var check was removed). Then `newConfig` does:
```go
if !af.v1ProtocolAvailable {
    c.internalConfig.SetTraceProtocol(traceProtocolV04, internalconfig.OriginCalculated)
}
if c.internalConfig.TraceProtocol() == traceProtocolV1 {
    // upgrade transport URL
}
```

This means: if the env var defaults to `"0.4"` (as hardcoded in `loadConfig`), and the agent supports v1, the protocol stays at v0.4 because the config initialized it to 0.4 and the code only downgrades, never upgrades. **But** if the env var is unset and the `supported_configurations.json` default of `"1.0"` is used via declarative config, the tracer will attempt v1 even when the user never asked for it. The semantics are now entirely dependent on whether the default comes from the hardcoded `"0.4"` in `loadConfig` or the `"1.0"` in `supported_configurations.json`.

Additionally, the unconditional downgrade `SetTraceProtocol(traceProtocolV04, OriginCalculated)` when `!af.v1ProtocolAvailable` always fires, overwriting whatever was configured, even when the agent is disabled/unreachable. This is a functional regression: when the agent is disabled (stdout mode, CI visibility agentless), the old code left `traceProtocol` at its default (v0.4) without touching it. The new code explicitly writes v0.4 with `OriginCalculated`, which means telemetry now reports a config change event that didn't exist before.

**Note**: The current `main` branch has already been patched (likely in a follow-up PR) to guard both the downgrade and upgrade behind a check: `if c.internalConfig.TraceProtocol() == traceProtocolV1 && !af.v1ProtocolAvailable`. This confirms this was indeed a problem that needed fixing.

---

## Should Fix

### 2. `resolveAgentURL` does not replicate "set-but-empty" semantics of `AgentURLFromEnv`

**File**: `internal/config/config_helpers.go:97-121`

The old `AgentURLFromEnv` uses `env.Lookup` which distinguishes between "env var is set but empty" and "env var is not set". When `DD_AGENT_HOST=""` (set but empty), the old code explicitly treats it as unset (`providedHost = false`), then falls through to UDS detection. The new `resolveAgentURL` receives string values from `p.GetString("DD_AGENT_HOST", "")`. If the env var is set to an empty string, `GetString` returns `""`, and the function checks `if host != "" || port != ""` -- this correctly falls through to UDS detection since both are empty. So the behavior is accidentally preserved. However, this relies on `GetString` returning `""` for set-but-empty, which is a fragile assumption. The old code had explicit comments about this edge case; the new code has no comment or test for it.

### 3. No unit tests for `resolveAgentURL` or `resolveTraceProtocol`

**File**: `internal/config/config_helpers.go:80-144`

Two new functions with non-trivial branching logic (`resolveAgentURL` has 4 code paths, `resolveTraceProtocol` has 2) have zero dedicated unit tests. The old `AgentURLFromEnv` had its own test suite (`internal/agent_test.go:14`). The `resolveAgentURL` function should have test coverage for:
- DD_TRACE_AGENT_URL with http, https, unix, invalid scheme, and parse error
- DD_AGENT_HOST and DD_TRACE_AGENT_PORT combinations
- UDS auto-detection fallback
- The priority ordering between the three sources

### 4. `SetAgentURL` does not report telemetry when URL is nil

**File**: `internal/config/config.go:275-282`

```go
func (c *Config) SetAgentURL(u *url.URL, origin telemetry.Origin) {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.agentURL = u
    if u != nil {
        configtelemetry.Report("DD_TRACE_AGENT_URL", u.String(), origin)
    }
}
```

If `SetAgentURL(nil, ...)` is called, the URL is set to nil without any telemetry report. This creates an inconsistency: setting a value reports telemetry, clearing it does not. While nil may not be a realistic call site today, the API allows it. Consider either reporting the clear or documenting that nil is not a valid argument (e.g., panic or no-op).

### 5. `AgentURL()` returns nil when `agentURL` is nil, which will panic callers

**File**: `internal/config/config.go:287-293`

```go
func (c *Config) AgentURL() *url.URL {
    u := c.RawAgentURL()
    if u != nil && u.Scheme == "unix" {
        return internal.UnixDataSocketURL(u.Path)
    }
    return u
}
```

If `agentURL` is nil (e.g., during test setup or before initialization), `AgentURL()` returns nil. All existing call sites (e.g., `c.internalConfig.AgentURL().String()` in `civisibility_transport.go:109`, `telemetry.go:55`, `tracer.go:271`) will nil-pointer panic. The old code had a similar issue but the field was never nil in practice because `newConfig` always set a default via `AgentURLFromEnv`. The new `loadConfig` also always sets a default, but the `CreateNew()` / test setup path could leave it nil if the provider returns unexpected values.

### 6. `internal.AgentURLFromEnv()` is now partially duplicated but not deprecated

**Files**: `internal/agent.go:44-86`, `internal/config/config_helpers.go:91-144`

`resolveAgentURL` reimplements the same logic as `AgentURLFromEnv` with minor differences (reads strings from provider vs. calling `env.Get`/`env.Lookup` directly). But `AgentURLFromEnv` is still called by other packages (`profiler/options.go:204`, `openfeature/exposure.go:190`, `internal/civisibility/utils/net/client.go:169`). This creates a maintenance burden: bug fixes to one must be mirrored in the other. The old function should be marked as deprecated or refactored to delegate to the shared logic.

---

## Nits

### 7. Inconsistent use of `telemetry.OriginCode` vs `internalconfig.OriginCode`

**Files**: `ddtrace/tracer/option.go:1001`, `ddtrace/tracer/option.go:1029`, etc.

Some call sites use `telemetry.OriginCode` (e.g., `WithAgentAddr`, `WithAgentURL`, `WithUDS`) while others use `internalconfig.OriginCode` (e.g., `civisibility_transport_test.go:91`). Both resolve to the same constant, but mixing the import paths makes it harder to grep for origin usage consistently. Pick one and use it throughout the tracer package.

### 8. Comment has a doc-comment formatting issue

**File**: `internal/config/config_helpers.go:93-96`

```go
//  3. DefaultTraceAgentUDSPath (if the socket file exists)
//  4. http://localhost:8126
```

Line 96 in the godoc comment block has `/ ` (forward-slash space) instead of `// ` (double-slash space). This would cause a malformed godoc rendering.

### 9. Exported constants `URLSchemeUnix`, `URLSchemeHTTP`, `URLSchemeHTTPS` may be overly broad

**File**: `internal/config/config_helpers.go:70-73`

These are very generic constant names exported from an `internal/config` package. They are only used within `resolveAgentURL` and `resolveOTLPTraceURL`. Consider keeping them unexported (lowercase) since they are internal implementation details.

### 10. `TraceMaxSize` rename from `traceMaxSize` is unrelated to PR scope

**File**: `internal/config/config_helpers.go:55`

The diff shows `traceMaxSize` was renamed to `TraceMaxSize` (exported). This appears unrelated to the agentURL/traceProtocol migration and may deserve its own commit or at least a mention in the PR description.

### 11. `GetStringWithValidator` silently falls back to default on invalid values

**File**: `internal/config/provider/provider.go:84-90`

When `validate` returns false, the function returns the default value without logging a warning. For `DD_TRACE_AGENT_PROTOCOL_VERSION`, if a user sets it to an invalid value like `"2.0"`, it silently falls back to `"0.4"` with no indication. The old code path in `fetchAgentFeatures` simply did not match `"1.0"` and left the protocol at v0.4, which was also silent -- but now that this is a first-class config knob read at startup, a warning would be more helpful.

### 12. The `GetURL` method was removed from the provider but tests still reference it in comments

**File**: `internal/config/provider/provider.go` (deleted `GetURL`), `internal/config/provider/provider_test.go`

The `GetURL` removal is clean, but some test adjustments simply changed `GetURL(...)` to `GetString(...)` assertions. The test at `provider_test.go:730` now asserts `"https://localhost:8126"` as a plain string, which loses type safety compared to the old `*url.URL` assertion. This is acceptable but worth noting.
