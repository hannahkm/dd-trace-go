# PR #4483: refactor(config): migrate peer service config to internal/config

## Summary
This PR moves peer service configuration (`peerServiceDefaultsEnabled` and `peerServiceMappings`) from the tracer's local `config` struct to the global `internal/config.Config` singleton, adding proper getter/setter methods with mutex protection. It also changes `TracerConf.PeerServiceMappings` from a `map[string]string` to a `func(string) (string, bool)` lookup function to avoid per-call map copies on the hot path. The span attribute schema parsing is also moved into `internal/config` with a new `parseSpanAttributeSchema` helper. Telemetry reporting is wired through the new setters.

---

## Blocking

1. **`TracerConf.PeerServiceMapping` is a public API-breaking change**
   - Files: `ddtrace/tracer/tracer.go`, `ddtrace/tracer/api.txt`
   - `TracerConf.PeerServiceMappings` was `map[string]string` and is now `PeerServiceMapping func(string) (string, bool)`. This is a breaking change to the public `TracerConf` struct. Any code that reads `TracerConf.PeerServiceMappings` (including contrib packages, user code, or other Datadog libraries) will fail to compile. The `api.txt` file confirms this is part of the public API surface. This needs careful consideration:
     - Is there a deprecation policy for this struct?
     - Should both fields coexist temporarily (old field deprecated, new field added)?
     - At minimum, this should be called out in release notes as a breaking change.

2. **`PeerServiceMapping` method is bound to `Config` receiver but stored as a function in `TracerConf` -- closure captures a mutable receiver**
   - File: `ddtrace/tracer/tracer.go`, line `PeerServiceMapping: t.config.internalConfig.PeerServiceMapping`
   - The `TracerConf` struct stores `PeerServiceMapping` as a reference to the *method* `Config.PeerServiceMapping`. This means every call to `tc.PeerServiceMapping("key")` goes through `c.mu.RLock()` / `c.mu.RUnlock()` in the `Config` receiver. While this is thread-safe, it means the `TracerConf` value is not a snapshot -- it reflects the current state of the config at call time, not at `TracerConf()` creation time. This is inconsistent with all other `TracerConf` fields which are value snapshots. If someone calls `SetPeerServiceMapping` between when `TracerConf()` was called and when `PeerServiceMapping` is invoked, the result changes. This could lead to subtle bugs.

---

## Should Fix

1. **`SetPeerServiceMappings` and `SetPeerServiceMapping` hold the lock while building telemetry strings**
   - File: `internal/config/config.go`, `SetPeerServiceMappings` and `SetPeerServiceMapping`
   - In `SetPeerServiceMapping`, the lock is held while iterating over the map and building the telemetry string (`fmt.Sprintf`, `strings.Join`). While the map is typically small, holding a write lock during string formatting is unnecessary. The `SetPeerServiceMappings` method does release the lock before calling `configtelemetry.Report`, but `SetPeerServiceMapping` also releases it before the report. However, building the `all` slice happens under the lock. Consider building the telemetry string after releasing the lock, using the copy pattern from `PeerServiceMappings()`.

2. **`parseSpanAttributeSchema` only accepts "v0" and "v1" but the old code used `p.GetInt`**
   - File: `internal/config/config_helpers.go`, `parseSpanAttributeSchema`
   - The old code parsed `DD_TRACE_SPAN_ATTRIBUTE_SCHEMA` as an integer (0, 1). The new code parses it as a string ("v0", "v1"). This is a behavioral change: users who had `DD_TRACE_SPAN_ATTRIBUTE_SCHEMA=1` (integer form) will now get a warning and fallback to v0 instead of using v1. This is a silent regression for existing users. The function should also accept plain "0" and "1" for backward compatibility.

3. **`Config.peerServiceMappings` is loaded from env in `loadConfig` but also conditionally set in the new schema logic -- potential ordering issue**
   - File: `internal/config/config.go`, `loadConfig`
   - The env var `DD_TRACE_PEER_SERVICE_MAPPING` is loaded at line `cfg.peerServiceMappings = p.GetMap(...)`, then later `DD_TRACE_PEER_SERVICE_DEFAULTS_ENABLED` is loaded. After that, a new block checks `cfg.spanAttributeSchemaVersion >= 1` and sets `cfg.peerServiceDefaultsEnabled = true`. However, the old code in `option.go` also had `c.peerServiceDefaultsEnabled = internal.BoolEnv("DD_TRACE_PEER_SERVICE_DEFAULTS_ENABLED", false)` followed by a schema version check. The migration to `loadConfig` must preserve the same precedence: if `DD_TRACE_PEER_SERVICE_DEFAULTS_ENABLED=false` is explicitly set by the user AND the schema is v1, what wins? In the old code, the env var was read first, then schema v1 overrode it to `true`. In the new code, `p.GetBool("DD_TRACE_PEER_SERVICE_DEFAULTS_ENABLED", false)` is read, then schema v1 overwrites it. So the schema v1 always wins, which matches the old behavior. This is correct but should be documented with a comment.

4. **`PeerServiceMapping` in `Config` does not use `defer` for `RUnlock` -- could panic-leak if map lookup panics**
   - File: `internal/config/config.go`, `PeerServiceMapping` method
   - The method manually calls `c.mu.RUnlock()` instead of using `defer`. While a map lookup on a non-nil map should never panic, if the function is ever extended (e.g., with additional logic), forgetting to unlock is a risk. The comment says this avoids per-call allocation, but `defer` in modern Go (1.14+) is essentially free for simple cases. Consider using `defer` for safety.

5. **Test `TestCiVisibilityNoopTracer_TracerConf` now compares fields individually but misses `PeerServiceMapping`**
   - File: `ddtrace/tracer/civisibility_nooptracer_test.go`
   - The test comment says "functions can't be compared with reflect.DeepEqual" and compares all fields individually except `PeerServiceMapping`. However, it also does not test that `PeerServiceMapping` behaves the same between the wrapped and unwrapped tracer. At minimum, test that both return the same result for a known key.

---

## Nits

1. **`parseSpanAttributeSchema` returns `(int, bool)` but the second return is only used to detect invalid values**
   - File: `internal/config/config_helpers.go`
   - The function logs a warning internally when the value is invalid. The caller in `loadConfig` checks `ok` but does nothing with it (just skips the set). Consider whether the warning log is sufficient or if the caller should also log/act on the failure.

2. **Inconsistent naming: `PeerServiceMapping` (singular, function) vs `PeerServiceMappings` (plural, map copy)**
   - File: `internal/config/config.go`
   - Both methods exist on `Config`. The singular form does a single lookup, the plural returns the full map. This is clear from the doc comments but could confuse callers at a glance. Consider renaming the singular to `LookupPeerServiceMapping` for clarity.

3. **The `api.txt` change confirms this is a public API modification**
   - File: `ddtrace/tracer/api.txt`
   - This file tracks the public API surface. The change from `PeerServiceMappings map[string]string` to `PeerServiceMapping func(string)(string, bool)` should be accompanied by a changelog entry.

4. **`SetPeerServiceMappings` makes a defensive copy of the input but `SetPeerServiceMapping` does not clone existing entries**
   - File: `internal/config/config.go`
   - `SetPeerServiceMappings` creates a new map and copies. `SetPeerServiceMapping` modifies the existing map in place. If the initial map was set via `loadConfig` (from env parsing), the map reference may be shared. This is likely safe since `loadConfig` creates a fresh map, but it is worth noting the asymmetry.
