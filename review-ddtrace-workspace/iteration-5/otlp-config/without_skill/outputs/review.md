# PR #4583: feat(config): add OTLP trace export configuration support

## Summary

This PR adds configuration support for OTLP trace export mode. When `OTEL_TRACES_EXPORTER=otlp` is set, the tracer uses a separate OTLP collector endpoint and OTLP-specific headers instead of the standard Datadog agent trace endpoint. This is configuration groundwork only -- actual OTLP serialization is deferred to a follow-up PR.

Key changes:
- Adds `otlpExportMode`, `otlpTraceURL`, and `otlpHeaders` fields to `internal/config.Config`, loaded from `OTEL_TRACES_EXPORTER`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, and `OTEL_EXPORTER_OTLP_TRACES_HEADERS`.
- `DD_TRACE_AGENT_PROTOCOL_VERSION` takes precedence over `OTEL_TRACES_EXPORTER` when both are set.
- Refactors `newHTTPTransport` to accept pre-resolved `traceURL`, `statsURL`, and `headers` (making it protocol-agnostic).
- Extracts `resolveTraceTransport()` and `datadogHeaders()` functions.
- Updates `mapEnabled` in `otelenvconfigsource.go` to accept `"otlp"` as a valid `OTEL_TRACES_EXPORTER` value.
- `GetMap` in the config provider now accepts a delimiter parameter, supporting both DD-style (`:`) and OTel-style (`=`) delimiters.

**Key files changed:** `ddtrace/tracer/option.go`, `ddtrace/tracer/transport.go`, `ddtrace/tracer/tracer.go`, `internal/config/config.go`, `internal/config/config_helpers.go`, `internal/config/provider/provider.go`, `internal/config/provider/otelenvconfigsource.go`, and associated test files.

---

## Blocking

### 1. V1 protocol downgrade logic is broken when agent doesn't support V1

The original code:
```go
if !af.v1ProtocolAvailable {
    c.internalConfig.SetTraceProtocol(traceProtocolV04, ...)
}
if c.internalConfig.TraceProtocol() == traceProtocolV1 {
    if t, ok := c.transport.(*httpTransport); ok {
        t.traceURL = fmt.Sprintf("%s%s", agentURL.String(), tracesAPIPathV1)
    }
}
```

The new code:
```go
if c.internalConfig.TraceProtocol() == traceProtocolV1 && !af.v1ProtocolAvailable {
    c.internalConfig.SetTraceProtocol(traceProtocolV04, ...)
    if t, ok := c.transport.(*httpTransport); ok {
        t.traceURL = agentURL.String() + tracesAPIPath
    }
}
```

The original code had two separate `if` blocks: (1) downgrade to V04 if agent doesn't support V1, and (2) if still on V1 (agent supports it), set the V1 trace URL. The new code combines them into a single condition that only fires when the protocol is V1 AND the agent doesn't support it. This means: **when the agent DOES support V1, the trace URL is never updated to the V1 path.** The URL was already set by `resolveTraceTransport()` earlier, which does handle the V1 case. However, `resolveTraceTransport` is called before `loadAgentFeatures`, so it correctly uses the configured protocol. The net effect seems correct (V1 URL is set in `resolveTraceTransport`, and the downgrade block only fires to revert to V04 URL), but this is a logic refactor that changes when and how the URL is set. Verify with tests that the V1 protocol path still works end-to-end when the agent supports it.

---

## Should Fix

### 1. Stats URL still goes to Datadog agent in OTLP mode

In `resolveTraceTransport`, only the trace URL is resolved for OTLP mode. The stats URL is always set to `agentURL + statsAPIPath`:
```go
c.transport = newHTTPTransport(traceURL, agentURL+statsAPIPath, c.httpClient, headers)
```

When running in OTLP mode without a Datadog agent (e.g., only an OTLP collector), the stats URL will point to a non-existent endpoint. If the tracer sends stats in OTLP mode, this will fail silently or produce errors. Consider whether stats should be disabled in OTLP mode or routed through the OTLP collector.

### 2. `OTLPHeaders()` returns a copy but `otlpTraceURL` does not

`OTLPHeaders()` correctly returns `maps.Clone(c.otlpHeaders)` to prevent mutation of the internal map. But `OTLPTraceURL()` returns the string directly, which is fine since strings are immutable in Go. This is consistent, just noting for completeness.

### 3. `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` does not append `/v1/traces` automatically

The `resolveOTLPTraceURL` function uses the user-provided URL as-is when it passes validation:
```go
if u.Scheme != URLSchemeHTTP && u.Scheme != URLSchemeHTTPS {
    // fallback
} else {
    return otlpTracesEndpoint  // used as-is
}
```

Per the OTel spec, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is a signal-specific endpoint that should be used as-is (unlike the base `OTEL_EXPORTER_OTLP_ENDPOINT` which requires appending `/v1/traces`). This behavior is correct per spec. However, if someone sets `OTEL_EXPORTER_OTLP_ENDPOINT` (without the `_TRACES` suffix) expecting it to work, it won't be picked up. Consider whether `OTEL_EXPORTER_OTLP_ENDPOINT` should also be supported as a fallback (with `/v1/traces` appended), per the OTel spec hierarchy.

### 4. `DD_TRACE_AGENT_PROTOCOL_VERSION` default changed in `supported_configurations.json`

The diff shows:
```json
"DD_TRACE_AGENT_PROTOCOL_VERSION": [
  {
    "implementation": "B",
    "type": "string",
    "default": "1.0"
  }
]
```

The default is listed as `"1.0"`, but the actual code default is `"0.4"` (as seen in `loadConfig` where `GetStringWithValidator` uses `TraceProtocolVersionStringV04`). If this JSON is auto-generated, ensure the generator picks up the correct default. If manually maintained, this appears to be an error.

### 5. `IsSet` on `Provider` re-queries all sources

The `IsSet` method iterates over all sources to check if a key has been set:
```go
func (p *Provider) IsSet(key string) bool {
    for _, source := range p.sources {
        if source.get(key) != "" {
            return true
        }
    }
    return false
}
```

The TODO comment acknowledges this should be tracked during initial iteration. More importantly, `IsSet` returning `true` for any non-empty value means that `DD_TRACE_AGENT_PROTOCOL_VERSION=""` (empty string) would return `false`, which is the correct behavior. However, if a source returns whitespace-only strings, those would be considered "set" which may not be intended.

### 6. `buildOTLPHeaders` overwrites user-provided `Content-Type`

```go
func buildOTLPHeaders(headers map[string]string) map[string]string {
    if headers == nil {
        headers = make(map[string]string)
    }
    headers["Content-Type"] = OTLPContentTypeHeader
    return headers
}
```

If the user sets `Content-Type` in `OTEL_EXPORTER_OTLP_TRACES_HEADERS`, it will be overwritten with `application/x-protobuf`. This is probably intentional (protobuf is the only supported encoding), but should be documented. A log warning when overwriting a user-provided Content-Type would be helpful.

---

## Nits

### 1. Typo in constant name: `OtelTagsDelimeter`

The constant referenced as `internal.OtelTagsDelimeter` has a typo -- it should be `OtelTagsDelimiter` (with an 'i' before the second 'e'). This appears to be a pre-existing issue, not introduced by this PR.

### 2. `resolveTraceTransport` is in `option.go` but `resolveOTLPTraceURL` is in `config_helpers.go`

The URL resolution logic is split across two packages/files. `resolveTraceTransport` in `option.go` decides between OTLP and Datadog mode and calls `resolveOTLPTraceURL` in `config_helpers.go`. This works but makes the trace URL resolution logic harder to follow. Consider whether both functions belong in the same file.

### 3. Test coverage for `buildOTLPHeaders` with nil input

The test for `OTLPHeaders` when no env var is set verifies `Content-Type` is present and there's exactly 1 header. This implicitly tests the `nil` input path of `buildOTLPHeaders`. Consider adding an explicit unit test for `buildOTLPHeaders` directly.

### 4. `mapEnabled` switch statement formatting

The refactored switch in `otelenvconfigsource.go` is clean and easier to read than the previous if-else chain. Good improvement.
