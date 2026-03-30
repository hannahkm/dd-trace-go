# Review: PR #4583 — feat(config): add OTLP trace export configuration support

## Summary

This PR adds configuration support for OTLP trace export mode. When `OTEL_TRACES_EXPORTER=otlp` is set, the tracer resolves a separate OTLP collector endpoint and OTLP-specific headers instead of the standard Datadog agent trace endpoint. Key changes: (1) moves `otlpExportMode` from the tracer-level `config` struct into `internal/config.Config` with proper env var loading, (2) introduces `otlpTraceURL` and `otlpHeaders` fields resolved from `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` and `OTEL_EXPORTER_OTLP_TRACES_HEADERS`, (3) refactors `newHTTPTransport` to accept fully resolved `traceURL`, `statsURL`, and `headers` (making it protocol-agnostic), (4) adds `resolveTraceTransport()` to select between Datadog and OTLP modes, (5) makes `DD_TRACE_AGENT_PROTOCOL_VERSION` override `OTEL_TRACES_EXPORTER`, and (6) adds `parseMapString` delimiter parameter to support OTel's `=` delimiter alongside DD's `:` delimiter.

## Reference files consulted

- style-and-idioms.md (always)
- concurrency.md (config fields accessed under mutex)

## Findings

### Blocking

1. **`resolveTraceTransport` is called before agent feature detection, but agent feature detection may downgrade the protocol and overwrite `traceURL`** (`option.go:420-423` vs `option.go:461-466`). The transport is created at line 422 with `resolveTraceTransport(c.internalConfig)`, which selects the trace URL based on the current `traceProtocol`. Then at line 461, agent feature detection may downgrade v1 to v0.4 and update `t.traceURL`. However, the downgrade logic now has a bug: `t.traceURL = agentURL.String() + tracesAPIPath` uses `tracesAPIPath` (v0.4 path) as the downgrade target, which is correct. But this only executes when `TraceProtocol() == traceProtocolV1 && !af.v1ProtocolAvailable` — if the protocol was already v0.4 (the default), this block is skipped entirely, which is correct. In OTLP mode, `traceProtocol` would be the default v0.4 (OTLP mode doesn't change it), so the block is skipped, and the OTLP URL survives. This appears correct on closer inspection.

   **On reflection, the flow is sound.** Not a blocking issue.

### Should fix

1. **`buildOTLPHeaders` always sets `Content-Type: application/x-protobuf`, even if user provided a different Content-Type** (`config_helpers.go:181`). The function unconditionally overwrites `headers["Content-Type"]`. If a user sets `OTEL_EXPORTER_OTLP_TRACES_HEADERS=Content-Type=application/json,...`, their value would be silently overwritten. This is probably intentional (protobuf is the required format), but the behavior should be documented in the function comment, e.g., "Content-Type is always set to application/x-protobuf regardless of user-provided headers."

2. **`resolveOTLPTraceURL` falls back to localhost when agent URL is a UDS socket** (`config_helpers.go:166-170`). When the agent URL is `unix:///var/run/datadog/apm.socket`, `rawAgentURL.Hostname()` returns an empty string, so the fallback is `localhost`. This is tested and documented. However, the warning messages for invalid URLs use `log.Warn` from the `internal/log` package, which may not be initialized yet at config load time (line 157). Verify that logging is available when `loadConfig` runs.

3. **`OTLPHeaders` returns a `maps.Clone` — good, but `datadogHeaders()` allocates a new map every call** (`transport.go:78,215`). `datadogHeaders()` is called from `resolveTraceTransport` (once at init) and from test helpers. Since it is init-time only, this is fine. But the function also calls `internal.ContainerID()`, `internal.EntityID()`, and `internal.ExternalEnvironment()` on every invocation. If these are expensive (they involve file reads or cgroup parsing), consider caching the result. This is minor since it is init-time.

4. **`tracesAPIPath` vs `TracesPathV04` naming inconsistency** (`config_helpers.go:39-40` and `option.go`). The PR introduces `TracesPathV04` and `TracesPathV1` as exported constants in `config_helpers.go`, but the tracer code in `option.go` still uses local unexported constants `tracesAPIPath` and `tracesAPIPathV1`. These should either be unified (tracer imports the `config` constants) or the `config` constants should be unexported if they are not needed outside the package. Having two sets of constants for the same paths is confusing and invites drift.

5. **`OTEL_TRACES_EXPORTER` is read twice in different places** (`config.go:168` and `otelenvconfigsource.go:134`). In `loadConfig`, `cfg.otlpExportMode = p.GetString("OTEL_TRACES_EXPORTER", "") == "otlp"`. In `mapEnabled`, `OTEL_TRACES_EXPORTER=otlp` now returns `"true"` (maps to `DD_TRACE_ENABLED=true`). These are consistent, but the dual reading means the semantics of `OTEL_TRACES_EXPORTER` are split across two files. Consider adding a comment in `loadConfig` cross-referencing `mapEnabled` to make the full picture clear.

6. **`parseMapString` now requires a delimiter parameter but the comment says "prioritizes the Datadog delimiter (:) over the OTel delimiter (=)"** (`provider.go:178-179`). This comment is misleading — the function does not prioritize anything; it uses whatever delimiter is passed. The old behavior hardcoded `:`. The comment should be updated to say "parses a string containing key-value pairs using the given delimiter."

7. **`DD_TRACE_AGENT_PROTOCOL_VERSION` default changed from `"1.0"` to `"0.4"` in `supported_configurations.json`** wait, actually looking at the JSON diff, it appears the entry was moved but the default is still `"1.0"`. The constant `TraceProtocolVersionStringV04 = "0.4"` is used in `loadConfig` as the default for `GetStringWithValidator`. Verify that the JSON metadata default (`"1.0"`) matches the code default (`"0.4"`). If they disagree, documentation consumers will get confused.

### Nits

1. **`fmt.Sprintf` used for URL construction in `resolveOTLPTraceURL`** (`config_helpers.go:172`). `fmt.Sprintf("http://%s:%s%s", host, otlpDefaultPort, otlpTracesPath)` is init-time code, so performance is not a concern. But per style-and-idioms, simple string concatenation (`"http://" + host + ":" + otlpDefaultPort + otlpTracesPath`) is preferred for clarity. Minor nit.

2. **Typo: `OtelTagsDelimeter` in config.go** (`config.go:174`). `internal.OtelTagsDelimeter` — "Delimeter" is a common misspelling of "Delimiter". This is an existing constant name, not introduced by this PR, so not blocking.

3. **Empty line after closing brace in `TestOTLPHeaders`** (`config_test.go:618`). There is a blank line between the closing `}` of the last subtest and the closing `}` of the test function. Minor formatting.

## Overall assessment

Well-structured configuration groundwork for OTLP export. The separation of concerns is clean: `internal/config` owns the env var parsing and URL resolution, `resolveTraceTransport` bridges config to the transport layer, and `newHTTPTransport` is now protocol-agnostic. The `DD_TRACE_AGENT_PROTOCOL_VERSION` override of `OTEL_TRACES_EXPORTER` is a sensible precedence rule. Test coverage is thorough, covering default behavior, env var overrides, precedence, UDS fallback, invalid schemes, and the `mapEnabled` changes. The main concerns are the `TracesPathV04`/`tracesAPIPath` constant duplication, the misleading `parseMapString` comment, and the `supported_configurations.json` default discrepancy.
