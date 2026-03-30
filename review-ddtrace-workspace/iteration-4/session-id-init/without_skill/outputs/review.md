# Code Review: PR #4574 -- feat(telemetry): add stable session identifier headers

**PR**: https://github.com/DataDog/dd-trace-go/pull/4574
**Author**: khanayan123
**Branch**: `ayan.khan/stable-session-id-headers`

## Summary

This PR implements the Stable Service Instance Identifier RFC for Go instrumentation telemetry. It adds `DD-Session-ID` (always present, set to `runtime_id`) and `DD-Root-Session-ID` (present only in child processes) headers to telemetry requests. The root session ID is propagated to child processes via the `_DD_ROOT_GO_SESSION_ID` environment variable, set in the process environment during package initialization so that children spawned via `os/exec` inherit it automatically.

---

## Blocking

*No blocking issues found.*

---

## Should Fix

### 1. `make(http.Header, 11)` capacity is stale -- should be at least 14
**File**: `internal/telemetry/internal/writer.go:144`

The `make(http.Header, 11)` pre-allocation was sized for the original 11 static headers. This PR adds `DD-Session-ID` (always) and `DD-Root-Session-ID` (conditional), bringing the total to 12 static entries in the map + 2 conditional headers (`DD-Root-Session-ID`, `DD-Telemetry-Debug-Enabled`) = 14 possible headers. The undersized hint means the map may need to grow at runtime.

```go
// Current:
clonedEndpoint.Header = make(http.Header, 11)

// Should be:
clonedEndpoint.Header = make(http.Header, 14)
```

### 2. `TestRootSessionID_DefaultsToRuntimeID` depends on package-level init order and test isolation
**File**: `internal/globalconfig/globalconfig_test.go:27-30`

This test accesses `cfg.runtimeID` and `cfg.rootSessionID` directly (unexported struct fields) and asserts they are equal. This works today because the test binary is a root process (no `_DD_ROOT_GO_SESSION_ID` in env). However, if another test in the same package (or a parallel test run) sets `_DD_ROOT_GO_SESSION_ID` in the process environment before this test runs, the assertion would break because `cfg` is initialized once at package-load time from `newConfig()`, which reads the env var. Since `cfg` is a package-level `var`, it is only initialized once, so the risk is limited to the environment at process start, but this implicit dependency on test execution environment is fragile. Consider adding a `t.Setenv` guard that explicitly unsets `_DD_ROOT_GO_SESSION_ID`, or document the assumption.

### 3. `TestPreBakeRequest_SessionHeaders` does not actually exercise the child-process code path
**File**: `internal/telemetry/internal/writer_test.go:317-344`

The test has an `if/else` branch to check whether `DD-Root-Session-ID` is present or absent, but when run as a normal test (not a child subprocess), `RootSessionID() == RuntimeID()` is always true, so only the "absent" branch ever executes. The "inherited from parent" branch (line 341-342) is dead code in practice. To get real coverage of the child-process header path, the test would need to be run as a subprocess with `_DD_ROOT_GO_SESSION_ID` set (similar to what the globalconfig tests do). Same issue applies to `TestWriter_Flush_SessionHeaders` at line 346-390.

### 4. Using `env.Get` for an internal `_DD_` prefixed env var is semantically misleading
**File**: `internal/globalconfig/globalconfig.go:36`

`env.Get` is the canonical wrapper for reading *user-facing* configuration environment variables. It validates against `SupportedConfigurations` and auto-registers unknown vars in test mode. The `_DD_ROOT_GO_SESSION_ID` env var intentionally bypasses the validation check because it starts with `_DD_` (underscore prefix, not `DD_`), so it works correctly. However, using `env.Get` here is misleading because it implies this is a supported user-facing configuration variable. Using `os.Getenv` directly (with a `//nolint:forbidigo` directive and a comment explaining why) would be more semantically correct and self-documenting for an internal propagation mechanism. The `forbidigo` linter rule only forbids `os.Getenv` and `os.LookupEnv`, and `os.Setenv` is already used one line below without a nolint comment.

---

## Nits

### 1. The `newConfig()` function could benefit from a one-line doc comment
**File**: `internal/globalconfig/globalconfig.go:25`

Every other exported and unexported function in this file has a doc comment. Adding a brief comment like `// newConfig creates the initial global configuration` would be consistent.

### 2. Consider validating the inherited `_DD_ROOT_GO_SESSION_ID` value
**File**: `internal/globalconfig/globalconfig.go:35-42`

`getRootSessionID` trusts whatever string is in the environment variable without any validation. If a user or a misbehaving parent process sets `_DD_ROOT_GO_SESSION_ID` to an invalid value (empty after trimming, malformed, excessively long), it would be propagated as-is. A lightweight check (e.g., non-empty after `strings.TrimSpace`, maybe a length bound or UUID format check) could prevent silent propagation of garbage values.

### 3. Subprocess tests write JSON to stderr -- consider stdout instead
**File**: `internal/globalconfig/globalconfig_test.go:43,68`

The subprocess tests write their JSON output to `os.Stderr`. While this works (and avoids interference with test framework output on stdout), it is slightly unusual. If the subprocess panics or emits Go runtime errors, those also go to stderr and could corrupt the JSON, causing the `json.Unmarshal` to fail with a confusing error. Writing to stdout (and capturing `cmd.Stdout`) would be slightly more robust.

### 4. Minor: 15 commits for a small change
The PR has 15 commits for what amounts to ~30 lines of production code. Many are review-response fixups (extracting constants, renaming, removing nolint). Squashing before merge would keep history clean.

### 5. `DD-Telemetry-Request-Type` header not counted in capacity hint
**File**: `internal/telemetry/internal/writer.go:198`

`DD-Telemetry-Request-Type` is set in `newRequest()` via `Header.Set()`, not in `preBakeRequest()`. Since `preBakeRequest` clones the endpoint and the header map is shared, the capacity hint in `make(http.Header, 11)` should technically account for this header too. This is very minor since Go maps grow dynamically, but for completeness the hint should reflect all headers that will eventually be set on the request.
