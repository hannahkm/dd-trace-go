# Review: PR #4550 - refactor(config): Migrate agentURL and traceProtocol

**PR**: https://github.com/DataDog/dd-trace-go/pull/4550
**Author**: mtoffl01
**Status**: Merged

## Summary

This PR migrates the `agentURL` and `traceProtocol` fields from `ddtrace/tracer/config` into `internal/config/Config`, following the config revamp pattern. The key changes:

1. Removes `agentURL`, `originalAgentURL`, and `traceProtocol` fields from the tracer-level `config` struct.
2. Adds `RawAgentURL()`, `AgentURL()`, `SetAgentURL()`, `TraceProtocol()`, `SetTraceProtocol()` methods to `internal/config/Config`.
3. Moves URL resolution logic from `internal.AgentURLFromEnv()` (which uses raw `env.Lookup`) into `resolveAgentURL()` in `internal/config/config_helpers.go`, reading env vars through the provider so telemetry is reported.
4. Moves `DD_TRACE_AGENT_PROTOCOL_VERSION` reading into `loadConfig()`.
5. Replaces `Provider.GetURL()` with `Provider.GetStringWithValidator()` since the URL construction is now handled by `resolveAgentURL`.
6. `AgentURL()` now handles the UDS rewriting (unix -> http://UDS_...) at the config layer rather than mutating the stored URL in-place.
7. In `fetchAgentFeatures`, the env var check for `DD_TRACE_AGENT_PROTOCOL_VERSION` is removed; the feature now unconditionally reports `v1ProtocolAvailable = true` when the agent advertises `/v1.0/traces`, and the protocol is downgraded later if needed.

## Blocking

**1. Behavioral change in `fetchAgentFeatures` merits a closer look at the interaction with `loadConfig` initialization order** (`ddtrace/tracer/option.go:758`, `internal/config/config.go:154`)

The old code: `fetchAgentFeatures` only set `v1ProtocolAvailable = true` when both the agent advertised `/v1.0/traces` AND `DD_TRACE_AGENT_PROTOCOL_VERSION=1.0`. Then `newConfig` set `c.traceProtocol = traceProtocolV1` only when `v1ProtocolAvailable` was true.

The new code: `loadConfig` reads `DD_TRACE_AGENT_PROTOCOL_VERSION` and initializes `traceProtocol` to `TraceProtocolV1` if set to `"1.0"`. Then `fetchAgentFeatures` unconditionally sets `v1ProtocolAvailable = true` when the agent advertises `/v1.0/traces`. Then `newConfig` downgrades to v0.4 only if the agent does NOT support v1.

The net effect is the same: v1 is used only when the env var says `1.0` AND the agent supports it. However, the new path also sets the v1 trace URL when `traceProtocol == v1` and `v1ProtocolAvailable` is true, which is a slightly different code path. The concern is: if the transport was already created with the default v0.4 URL (line 420-423), and then the protocol is NOT downgraded, the transport URL is never upgraded to v1. Looking at the diff lines 458-467:

```go
agentURL := c.internalConfig.AgentURL()
af := loadAgentFeatures(agentDisabled, agentURL, c.httpClient)
c.agent.store(af)
// If the agent doesn't support the v1 protocol, downgrade to v0.4
if !af.v1ProtocolAvailable {
    c.internalConfig.SetTraceProtocol(traceProtocolV04, internalconfig.OriginCalculated)
}
if c.internalConfig.TraceProtocol() == traceProtocolV1 {
    if t, ok := c.transport.(*httpTransport); ok {
        t.traceURL = fmt.Sprintf("%s%s", agentURL.String(), tracesAPIPathV1)
    }
}
```

In the old code, the v1 URL was set inside the `if af.v1ProtocolAvailable` block. In the new code, the v1 URL is set when `TraceProtocol() == traceProtocolV1` (which is only possible if the env var was set to 1.0 AND the agent supports v1, since the downgrade runs first). This is semantically equivalent but the two-step logic is less obvious than the old single-branch approach. Not a bug, but the reasoning requires careful reading.

## Should Fix

**1. Missing unit tests for `resolveAgentURL`, `resolveTraceProtocol`, and `validateTraceProtocolVersion`** (`internal/config/config_helpers.go:80-121`)

The `resolveAgentURL` function contains significant URL resolution logic (DD_TRACE_AGENT_URL priority, DD_AGENT_HOST/DD_TRACE_AGENT_PORT fallback, UDS detection, error handling for invalid URLs/schemes). This logic was previously tested indirectly via `internal.AgentURLFromEnv` tests, but the new standalone function has zero dedicated test coverage. Codecov confirms `config_helpers.go` is at 43.24% patch coverage with 17 missing and 4 partial lines. A table-driven test for `resolveAgentURL` covering the priority order (explicit URL > host/port > UDS > default) and error cases (invalid scheme, parse error) would catch regressions during future refactoring.

Similarly, `resolveTraceProtocol` and `validateTraceProtocolVersion` have no unit tests.

**2. `resolveAgentURL` duplicates logic from `internal.AgentURLFromEnv` without deprecating or removing the original** (`internal/config/config_helpers.go:91-121`, `internal/agent.go:44-86`)

The PR creates a second implementation of agent URL resolution that mirrors `internal.AgentURLFromEnv` but reads from provider strings instead of `env.Lookup`. Both implementations must be kept in sync if the resolution logic changes. A comment in one referencing the other (or a TODO to deprecate `AgentURLFromEnv` once the migration is complete) would help prevent drift.

**3. `GetStringWithValidator` silently falls back to default on invalid values without logging** (`internal/config/provider/provider.go:84-91`)

When `validate` returns false, the function returns `("", false)` to `get()`, which falls through to the default. For `DD_TRACE_AGENT_PROTOCOL_VERSION`, if a user sets an invalid value like `"2.0"`, the system silently uses `"0.4"` with no warning. `AgentURLFromEnv` logs when an unsupported scheme is encountered; this validator should similarly log when an unrecognized protocol version is rejected. This is the "don't silently drop errors" pattern from the review checklist.

**4. Happy path not left-aligned in `resolveAgentURL`** (`internal/config/config_helpers.go:99-109`)

The success case is nested inside `if err == nil { switch ... }` inside `if agentURLStr != "" { ... }`. The error case (`err != nil`) could use an early `continue`/`return` pattern to reduce nesting:

```go
if agentURLStr != "" {
    u, err := url.Parse(agentURLStr)
    if err != nil {
        log.Warn("Failed to parse DD_TRACE_AGENT_URL: %s", err.Error())
    } else {
        switch ...
    }
}
```

Could become:

```go
if agentURLStr != "" {
    u, err := url.Parse(agentURLStr)
    if err != nil {
        log.Warn(...)
        // fall through to host/port resolution
    } else if u.Scheme != URLSchemeUnix && u.Scheme != URLSchemeHTTP && u.Scheme != URLSchemeHTTPS {
        log.Warn(...)
        // fall through
    } else {
        return u
    }
}
```

This is a minor instance but given this is the single most common review comment in the repo, it's worth noting.

## Nits

**1. Exported constants `URLSchemeUnix`, `URLSchemeHTTP`, `URLSchemeHTTPS` may be premature API surface** (`internal/config/config_helpers.go:30-38`)

These are only used within the `config` package itself (in `resolveAgentURL`). Unless there are plans for other packages to reference them, keeping them unexported (`urlSchemeUnix`, etc.) follows the "don't add unused API surface" convention. The same applies to `TraceProtocolVersionStringV04` and `TraceProtocolVersionStringV1` -- they're only used by `validateTraceProtocolVersion` and `resolveTraceProtocol` within this package.

**2. `SetAgentURL` and `SetTraceProtocol` lack godoc comments** (`internal/config/config.go:275`, `internal/config/config.go:717`)

`RawAgentURL()` and `AgentURL()` have godoc explaining the difference between raw and effective URLs. `SetAgentURL` is exported but has no comment explaining that it stores the raw (pre-rewrite) URL. While other setters in this file also lack godoc (existing convention), the raw/effective URL distinction makes this one worth documenting since callers need to understand that `SetAgentURL` stores the raw form and `AgentURL()` rewrites UDS on read.

**3. `TraceProtocolV04 = 0.4` uses `float64` for a version identifier** (`internal/config/config_helpers.go:30-31`)

This is inherited from the old code, not introduced by this PR, but worth flagging during migration: using `float64` for protocol versions is fragile (floating point comparison `== 0.4` works here because the values are exact IEEE 754 representations, but it's a foot-gun for future version numbers). A `string` or `int` enum would be safer. Not actionable in this PR since it's a pre-existing pattern.

**4. Import grouping in `config_helpers.go`** (`internal/config/config_helpers.go:8-16`)

The imports are correctly grouped (stdlib, then Datadog packages). No issue here, just confirming.

## Overall Assessment

The PR cleanly moves `agentURL` and `traceProtocol` into `internal/config` following the established migration pattern. The UDS rewriting is now lazily applied in `AgentURL()` rather than mutating the stored URL, which is a good design improvement. The `RawAgentURL()` / `AgentURL()` split is well-conceived and the test for UDS (asserting both raw and effective URLs) is a nice addition. The behavioral change in `fetchAgentFeatures` is semantically equivalent to the old code. The main gaps are the missing unit tests for the new helper functions and the silent validation failure in `GetStringWithValidator`.
