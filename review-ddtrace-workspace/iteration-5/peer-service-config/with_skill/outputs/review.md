# Review: PR #4483 - Move peer service config to internal/config

## Summary

This PR migrates peer service configuration (`peerServiceDefaultsEnabled` and `peerServiceMappings`) from `ddtrace/tracer/option.go`'s `config` struct into `internal/config/config.go`'s `Config` struct. The key improvements are:

1. **Hot-path optimization**: `TracerConf.PeerServiceMappings` changes from `map[string]string` (which was copied from the config lock on every span via `TracerConf()`) to `PeerServiceMapping func(string) (string, bool)` -- a single-key lookup function that acquires `RLock`, does a map lookup, and releases. This avoids copying the entire mappings map on every span.
2. **Config through proper channels**: Peer service config now flows through `internal/config` with proper `Get`/`Set` methods, telemetry reporting, and mutex protection, rather than living as raw fields on the tracer config.
3. **Schema-aware defaults**: `DD_TRACE_SPAN_ATTRIBUTE_SCHEMA` parsing is consolidated so that `peerServiceDefaultsEnabled` is automatically set to true when schema >= v1, inside `loadConfig`.

## Applicable guidance

- style-and-idioms.md (all Go code)
- performance.md (hot path optimization for per-span config reads)
- concurrency.md (mutex discipline, lock contention)

---

## Blocking

1. **`PeerServiceMapping` on `TracerConf` is a function closure that captures `*Config` and acquires `c.mu.RLock()` -- this is called on every span in `setPeerService`** (`config.go:688-697`, `spancontext.go:834`). While this is better than the previous approach of copying the entire map via `TracerConf()`, it still acquires an `RLock` on every span that has peer service tags. Per performance.md: "We are acquiring the lock and iterating over and copying internalconfig's PeerServiceMappings map on every single span, just to ultimately query the map by a key value." This PR addresses the "copying" part but still acquires the lock per span. For truly hot paths, consider whether the mappings can be cached in an `atomic.Pointer` (similar to the `atomicAgentFeatures` pattern) so reads are lock-free. Mappings only change via `WithPeerServiceMapping` at startup or via Remote Config, both of which are infrequent.

## Should fix

1. **`PeerServiceMapping` releases the RLock manually instead of using `defer`** (`config.go:689-697`). The function has two return paths and manually calls `c.mu.RUnlock()` in each. While this is technically correct and avoids `defer` overhead on the hot path, it is error-prone -- a future modification could add a return path that forgets to unlock. Per concurrency.md, when the critical section is this small (2 lines), the `defer` overhead is negligible compared to the lock acquisition itself. Consider using `defer` for safety, or add a comment explaining the deliberate `defer` avoidance for performance.

2. **`SetPeerServiceMappings` and `SetPeerServiceMapping` build telemetry strings under the lock** (`config.go:710-719`, `config.go:724-733`). Both functions iterate the map to build a telemetry string while holding `c.mu.Lock()`. The telemetry reporting (`configtelemetry.Report`) happens after the lock is released, which is good, but the string building (allocating `all` slice, `fmt.Sprintf` per entry, `strings.Join`) happens inside the critical section. Move the string building after the unlock:

    ```go
    c.mu.Lock()
    // ... mutate map ...
    snapshot := maps.Clone(c.peerServiceMappings)
    c.mu.Unlock()
    // build telemetry string from snapshot
    ```

3. **`PeerServiceMappings()` returns a full copy of the map, but the comment says "Not intended for hot paths"** (`config.go:670-679`). This is used in `startTelemetry` (called once at startup) which is fine. However, the old code in `option_test.go` still calls `c.internalConfig.PeerServiceMappings()` for test assertions (lines 891, 897, 907, 917), which returns a copy each time. This is fine for tests but worth noting that no production hot-path code should call this method.

4. **`parseSpanAttributeSchema` is defined in `config_helpers.go` but used only in `config.go`** (`config_helpers.go:57-69`). The function parses `"v0"`/`"v1"` strings. This is fine organizationally, but the function accepts empty string and returns `(0, true)`. However, the caller in `loadConfig` only calls it when the string is non-empty: `if schemaStr := p.GetString(...)` (line 170). So the empty-string case in `parseSpanAttributeSchema` is dead code. Either remove the empty-string handling from `parseSpanAttributeSchema`, or remove the non-empty check from the caller.

5. **The `api.txt` change indicates this is a public API change** (`api.txt:368`). Changing `PeerServiceMappings map[string]string` to `PeerServiceMapping func(string)(string, bool)` on `TracerConf` is a breaking change for any external code that reads `TracerConf.PeerServiceMappings`. The `TracerConf` struct is part of the public `Tracer` interface. Per contrib-patterns.md, resource name format changes can be breaking -- the same applies to public struct field type changes. Ensure this is documented in release notes or that `TracerConf` is not considered a stable public API.

6. **Test in `civisibility_nooptracer_test.go` manually compares fields instead of using `assert.Equal` on the struct** (`civisibility_nooptracer_test.go:241-249`). The comment explains this is because "functions can't be compared with reflect.DeepEqual." This is correct but fragile -- if new fields are added to `TracerConf`, this test won't automatically catch missing comparisons. Consider adding a helper that uses `reflect` to compare all fields except those of function type, or add a comment reminding future developers to update this test when adding new `TracerConf` fields.

## Nits

1. **Good use of `maps.Copy` for defensive copies** (`config.go:674,712`). This follows the standard library preference from style-and-idioms.md.

2. **Removed the `internal.BoolEnv` call for `DD_TRACE_PEER_SERVICE_DEFAULTS_ENABLED`** from `option.go` and replaced it with proper `internal/config` loading. This follows the config-through-proper-channels guidance from the universal checklist -- `internal.BoolEnv` is a raw `os.Getenv` wrapper that bypasses the validated config pipeline.

3. **The `loadConfig` logic that sets `peerServiceDefaultsEnabled = true` when schema >= 1** (`config.go:177-180`) is cleaner than the previous approach in `option.go` which used `internal.BoolEnv` with a conditional default. Good consolidation.

The code looks good overall. The primary win is eliminating the per-span map copy via the function-based lookup. The migration to `internal/config` is clean and follows the repo's config management patterns.
