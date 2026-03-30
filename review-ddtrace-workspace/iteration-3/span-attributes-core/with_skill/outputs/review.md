# Review: PR #4538 — Promote span fields out of meta map into a typed SpanAttributes struct

## Summary

This PR introduces `SpanAttributes` (a fixed-size array + bitmask for promoted fields) and `SpanMeta` (a wrapper combining the flat `map[string]string` with promoted attrs) to replace the plain `span.meta map[string]string`. It uses copy-on-write sharing of process-level attrs across spans and eliminates per-span allocations for promoted fields. The wire format is preserved via a hand-maintained msgp codec. The change is well-tested with both unit tests and benchmarks.

---

## Blocking

### 1. PR description and code disagree on which fields are promoted

The PR description and multiple comments/godoc strings reference four promoted fields (`env`, `version`, `component`, `span.kind`), but the actual `SpanAttributes` implementation only promotes **three**: `env`, `version`, `language`. `component` and `span.kind` are **not** in the `Defs` table, are not `AttrKey` constants, and `AttrKeyForTag` returns `AttrUnknown` for them (verified by the test at `span_attributes_test.go:421-422`). Meanwhile in `payload_v1.go`, `component` and `spanKind` are read via `span.meta.Get(ext.Component)` / `span.meta.Get(ext.SpanKind)` which routes through the flat map, not through promoted attrs.

This is not a correctness bug in the code (the code is internally consistent), but the PR description, struct-level godoc on `SpanMeta` (`span_meta.go:601-604`: "Promoted attributes (env, version, component, span.kind, language)"), field-level comment on `Span.meta` (`span.go:142-143`), and the test name `TestPromotedFieldsStorage` (which tests `component` and `span.kind` as if they were promoted when they are not stored in `SpanAttributes`) are all misleading. A reviewer or future contributor reading these comments would believe `component` and `span.kind` are in the bitmask struct when they live in the flat map.

**Why this matters:** Misleading documentation in a core data structure will cause incorrect assumptions during future changes. Either update the comments to say "env, version, language" or actually promote `component` and `span.kind` if that was the intent. The `TestPromotedFieldsStorage` test passes only because `meta.Get()` falls through to the flat map for non-promoted keys -- it does not actually verify promoted-field storage for `component`/`span.kind`.

(`ddtrace/tracer/internal/span_meta.go:601-604`, `ddtrace/tracer/span.go:142-143`, `ddtrace/tracer/span_test.go:560-585`)

### 2. `SpanAttributes.Set` is not nil-safe but other write methods are

`Set` (`span_attributes.go:176-179`) dereferences `a` without a nil check, while `Unset`, `Val`, `Has`, `Get`, `Count`, `Reset`, `All`, and `Clone` are all nil-safe. The godoc comment says "All read methods are nil-safe" but `Set` is a write method and will panic on a nil receiver. This is inconsistent with the rest of the API.

In `SpanMeta.ensureAttrsLocal()`, a nil `promotedAttrs` is handled by allocating a fresh `SpanAttributes` before calling `Set`, so the current call sites are safe. However, the asymmetry is a trap for future callers. Either add a nil guard to `Set` (allocating if nil, or documenting the panic contract), or add a godoc comment stating that `Set` requires a non-nil receiver.

(`ddtrace/tracer/internal/span_attributes.go:176-179`)

### 3. `init()` function in `span_meta.go` violates repo convention

The `init()` function at `span_meta.go:825-831` validates that `IsPromotedKeyLen` is in sync with `Defs`. This repo's style guide explicitly says `init()` is "very unpopular" and reviewers ask for named helper functions called from variable initialization instead. The compile-time guards in `span_attributes.go` (lines 153-157) already demonstrate the preferred pattern.

Consider replacing with a compile-time check or a `var _ = validatePromotedKeyLens()` pattern that runs at package init without using `init()`.

(`ddtrace/tracer/internal/span_meta.go:825-831`)

---

## Should Fix

### 4. `encodeMetaEntry` comment references "env/version/language" then "component and span.kind" inconsistently

The comment on `encodeMetaEntry` (`payload_v1.go:1166-1167`) says "env/version/language are encoded separately as fields 13-14/language; component and span.kind live in the flat map." But fields 13-16 encode env, version, component, and span.kind respectively (component is field 15, span.kind is field 16). The comment implies component and span.kind are only in the flat map, which contradicts their encoding as dedicated V1 fields. This will confuse anyone maintaining the V1 encoder.

(`ddtrace/tracer/payload_v1.go:1166-1167`)

### 5. Happy path not left-aligned in `SpanMeta.DecodeMsg`

In `DecodeMsg` (`span_meta.go:993-997`), the map reuse logic has the common case (map already allocated) in the `if` branch and the allocation in the `else`:

```go
if sm.m != nil {
    clear(sm.m)
} else {
    sm.m = make(map[string]string, header)
}
```

The left-aligned pattern would be:

```go
if sm.m == nil {
    sm.m = make(map[string]string, header)
} else {
    clear(sm.m)
}
```

This is a minor readability issue but it is the single most common review comment in this repo.

(`ddtrace/tracer/internal/span_meta.go:993-997`)

### 6. `BenchmarkSpanAttributesGet` map sub-benchmark reads `env` twice instead of `language`

In `span_attributes_test.go:492-494`, the map benchmark reads `m["env"]` twice and `m["language"]` once, while the `SpanAttributes` benchmark reads `env`, `version`, `language` each once. The comparison is not apples-to-apples. The map sub-benchmark should read `m["version"]` instead of the second `m["env"]`.

(`ddtrace/tracer/internal/span_attributes_test.go:492-494`)

### 7. `loadFactor` integer division truncates to 1

In `span_meta.go:592`, `loadFactor = 4 / 3` is integer division, which truncates to `1`. So `metaMapHint = expectedEntries * loadFactor = 5 * 1 = 5`, providing no slack at all. The comment says "~33% slack" but the actual hint is identical to `expectedEntries`. This is carried over from the old `initMeta()` function which had the same bug, but since this PR is moving the constants to a new location, it is a good time to fix it. Use `metaMapHint = expectedEntries * 4 / 3` (which gives 6) or define the hint directly.

(`ddtrace/tracer/internal/span_meta.go:590-593`)

### 8. `unsafe.Pointer` in mocktracer's `go:linkname` signature

The `spanStart` linkname signature in `mockspan.go` now takes `sharedAttrs unsafe.Pointer` instead of `*traceinternal.SpanAttributes`. The `unsafe` import changed from `_` to active. While this works, it means the mock tracer and the real tracer have divergent type safety at the call boundary -- the mock always passes `nil` and the types are not checked at compile time. If the `spanStart` signature ever changes (e.g., from pointer to value), the mock will silently pass `nil` without a compile error. Consider whether there is a way to import the actual type instead.

(`ddtrace/mocktracer/mockspan.go:19-23`)

### 9. Behavioral change in `srv_src_test.go` test assertions

In `srv_src_test.go`, the test `ChildInheritsSrvSrcFromParent` changed its assertion from `assert.Equal(t, serviceSourceManual, child.meta[ext.KeyServiceSource])` to `assert.Equal(t, "m", v)`. The value `"m"` is presumably the abbreviated form of `serviceSourceManual`, but this makes the test fragile -- if the constant value changes, the test hardcodes the current value rather than referencing the constant. Similarly, `ChildWithExplicitServiceGetsSrvSrc` uses `Source: "m"` instead of `Source: serviceSourceManual`.

(`ddtrace/tracer/srv_src_test.go:84-85, 99-101, 137-140`)

### 10. `ciVisibilityEvent.SetTag` no longer updates `Content.Meta` on each tag set

The `SetTag` method on `ciVisibilityEvent` removed the line `e.Content.Meta = e.span.meta` and deferred meta materialization to `Finish()`. While the `Finish()` method now correctly locks the span and calls `meta.Map()`, any code that reads `e.Content.Meta` between `SetTag` calls and `Finish()` will see stale data. The PR description does not mention whether CI Visibility consumers read `Content.Meta` between tag writes, but the removal of the per-tag update is a semantic change worth verifying.

(`ddtrace/tracer/civisibility_tslv.go:163-164, 209-214`)

### 11. Removal of `supportsLinks` field and native-links test

The PR removes the `supportsLinks` field from `Span` and deletes the `with_links_native` test case in `TestSpanLinksInMeta`. The `serializeSpanLinksInMeta` method previously skipped JSON serialization when `s.supportsLinks` was true (V1 protocol native links). Now it always serializes to JSON in meta. This changes behavior for V1 protocol spans -- they will now have both the native `span_links` field AND the `_dd.span_links` meta tag, potentially double-encoding links on the wire. This should be verified against the V1 encoder to confirm it is intentional.

(`ddtrace/tracer/span.go:849-856`, `ddtrace/tracer/span_test.go:1796-1810`)

---

## Nits

### 12. `for i := 0; i < b.N; i++` in benchmarks

Several benchmarks in `span_attributes_test.go` use the old `for i := 0; i < b.N; i++` pattern (lines 441, 453, 473, etc.) while others in the same file use `for range b.N` (line 556). The repo prefers `for range b.N` (Go 1.22+). Consider updating for consistency.

(`ddtrace/tracer/internal/span_attributes_test.go:441, 453, 473, etc.`)

### 13. `String()` method uses `fmt.Fprintf` in a loop

`SpanMeta.String()` (`span_meta.go:913-926`) uses `fmt.Fprintf(&b, "%s:%s", k, v)` inside a loop. Per the repo's performance guidance, `strings.Builder` with direct `WriteString` calls is preferred over `fmt.Sprintf`/`Fprintf` in paths that could be called frequently (debug logging). Consider:

```go
b.WriteString(k)
b.WriteByte(':')
b.WriteString(v)
```

(`ddtrace/tracer/internal/span_meta.go:922`)

### 14. Duplicated `mkSpan` helper in sampler tests

The `mkSpan` helper function is defined identically in four test functions (`TestPrioritySamplerRampCooldownNoReset`, `TestPrioritySamplerRampUp`, `TestPrioritySamplerRampDown`, `TestPrioritySamplerRampConverges`, `TestPrioritySamplerRampDefaultRate`) in `sampler_test.go`. While this duplication existed before this PR, the PR touches all of them to update the construction pattern. This would be a good time to extract a shared test helper.

(`ddtrace/tracer/sampler_test.go:2299-2306, 2312-2321, 2329-2336, 2343-2351, 2358-2366`)
