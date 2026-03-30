# PR #4538 Review: Promote redundant span fields into SpanAttributes

**PR**: https://github.com/DataDog/dd-trace-go/pull/4538
**Author**: darccio
**Branch**: `dario.castane/apmlp-856/promote-redundant-span-fields`

## Summary

This PR introduces `SpanAttributes` (a compact fixed-size struct for promoted V1-protocol fields: `env`, `version`, `language`) and `SpanMeta` (a replacement for `map[string]string` that routes promoted keys to `SpanAttributes` with copy-on-write semantics). The goal is to reduce per-span allocations and improve hot-path performance for the V1 protocol encoder.

---

## Blocking

### B1. `SpanAttributes.Set` panics on nil receiver

`ddtrace/tracer/internal/span_attributes.go:176-179`

Every other read method (`Val`, `Has`, `Get`, `Count`, `Unset`, `All`) is nil-safe, but `Set` is not. If `Set` is called on a nil `*SpanAttributes`, it will panic with a nil pointer dereference. While current callers appear to guard against this (via `ensureAttrsLocal` in `SpanMeta`), the inconsistency is dangerous -- any future caller who relies on the "nil-safe" pattern established by the other methods will hit a panic. Either add a nil guard or document that `Set` intentionally panics on nil (and add a compile-time or runtime check that callers never pass nil).

```go
func (a *SpanAttributes) Set(key AttrKey, v string) {
    // No nil check -- panics if a == nil
    a.vals[key] = v
    a.setMask |= 1 << key
}
```

### B2. `ciVisibilityEvent.SetTag` no longer updates `Content.Meta`, creating stale state

`ddtrace/tracer/civisibility_tslv.go:164-167`

The old code set `e.Content.Meta = e.span.meta` after every `SetTag` call, keeping `Content.Meta` in sync with the span's live metadata map. The new code removes that line, meaning `Content.Meta` is only populated at `Finish()` time. If any CI Visibility consumer reads `Content.Meta` between `SetTag` and `Finish` calls, it will see stale/empty data. The `Finish` method does lock and rebuild, but `Content.Metrics` is still updated eagerly in `SetTag` -- this asymmetry is confusing and suggests the `Meta` removal may have been unintentional. Verify that no code path reads `Content.Meta` before `Finish`.

### B3. `ciVisibilityEvent.Finish` acquires lock after `span.Finish` completes -- potential ordering issue

`ddtrace/tracer/civisibility_tslv.go:212-218`

The new `Finish` method calls `e.span.Finish(opts...)` first, then acquires `e.span.mu.Lock()` to rebuild `Content.Meta`. But `span.Finish` itself calls `s.meta.Finish()` (via `finishedOneLocked`), which sets `inlined=true` and may hand the span to the writer goroutine. After `span.Finish` returns, the writer could already be serializing `s.meta.m`. Acquiring the lock afterward and calling `s.meta.Map()` (which calls `Finish()` again, but is a no-op since `inlined` is already set) reads `s.meta.m` -- this is fine for Map() itself, but writing to `e.Content.Meta` and `e.Content.Metrics` could race with the serialization worker reading those same fields if `ciVisibilityEvent` is read concurrently. Verify that the CI visibility payload is not accessed by the writer goroutine before this lock/unlock completes, or move the rebuild into the span's `Finish` path under the trace lock.

---

## Should Fix

### S1. Semantic change in `deriveAWSPeerService` for S3 bucket names

`ddtrace/tracer/spancontext.go:937-939`

Old code: `if bucket := sm[ext.S3BucketName]; bucket != ""` -- checks both presence and non-emptiness.
New code: `if bucket, ok := sm.Get(ext.S3BucketName); ok` -- only checks presence, not emptiness.

If a span has `ext.S3BucketName` set to `""`, the old code would skip to `return "s3." + region + ".amazonaws.com"`, but the new code would use the empty bucket, producing `".s3." + region + ".amazonaws.com"` (note the leading dot). Fix by checking `ok && bucket != ""`:

```go
if bucket, ok := sm.Get(ext.S3BucketName); ok && bucket != "" {
```

### S2. PR description and multiple comments are stale -- mention `component`/`span.kind` as promoted fields

`ddtrace/tracer/internal/span_meta.go:36-37`, `ddtrace/tracer/span.go:141-143`, `ddtrace/tracer/internal/span_attributes.go` (Defs), various comments

The PR description says: "SpanAttributes -- a compact, fixed-size struct that stores the four V1-protocol promoted span fields (env, version, component, span.kind)". Multiple code comments still reference `component` and `span.kind` as promoted attributes:

- `span_meta.go:36`: "Promoted attributes (env, version, component, span.kind, language) live in attrs"
- `span.go:141-143`: "Promoted attributes (env, version, component, span.kind) live in meta.attrs"

But the actual `Defs` array contains only three entries: `env`, `version`, `language`. This is misleading and will confuse future maintainers. Update all comments to match the actual implementation.

### S3. `loadFactor = 4 / 3` is integer division, evaluates to 1, making `metaMapHint = 5`

`ddtrace/tracer/internal/span_meta.go:25-27`

```go
const (
    expectedEntries = 5
    loadFactor  = 4 / 3  // integer division: 4/3 = 1
    metaMapHint = expectedEntries * loadFactor  // = 5 * 1 = 5
)
```

The comment says "loadFactor of 4/3 (~1.33) provides ~33% slack", but Go integer division truncates `4/3` to `1`, so `metaMapHint` is just `5`, providing zero slack. This is copied from the old `initMeta()` function, so it is a pre-existing issue, but this is the opportunity to fix it. Either use a direct constant (e.g., `metaMapHint = 7`) or explicitly document that the "slack" is aspirational.

### S4. `TestPromotedFieldsStorage` tests `component` and `span.kind` as promoted but they are not

`ddtrace/tracer/span_test.go:2060-2085`

The test iterates over `ext.Environment`, `ext.Version`, `ext.Component`, `ext.SpanKind` and calls `span.meta.Get(tc.tag)`. Since `component` and `span.kind` are NOT promoted (they go to the flat map, not `SpanAttributes`), this test does not actually validate "promoted field storage" for those two keys. The test name is misleading. Either remove them from the test or rename the test to clarify it is testing "SetTag + Get round-trip" rather than promoted storage specifically.

### S5. `supportsLinks` field removed without clear justification

`ddtrace/tracer/span.go:165-166`, `ddtrace/tracer/span_test.go:2276-2292`

The `supportsLinks` field and the `with_links_native` test case are removed. The old code used `supportsLinks` to skip JSON serialization of span links into meta when native encoding was available. With the removal, `serializeSpanLinksInMeta` will now always serialize span links to meta, even when native encoding is supported -- meaning both the native encoder and the JSON-in-meta fallback produce data for the same span. Verify this is intentional and won't cause double-encoding of span links in V1 protocol payloads.

---

## Nits

### N1. Benchmark has 4 map reads but only 3 SpanAttributes reads

`ddtrace/tracer/internal/span_attributes_test.go:491-494`

The `map` sub-benchmark reads `m["env"]` twice (lines 492 and 494), giving 4 reads total, while the `SpanAttributes` sub-benchmark does only 3 reads. This makes the comparison unfair. Remove the duplicate `m["env"]` read:

```go
// line 493 should be:
s, ok = m["version"]
// line 494 reads m["env"] again -- should be m["language"]
s, ok = m["language"]
```

### N2. `ChildInheritsSrvSrcFromParent` test assertion weakened

`ddtrace/tracer/srv_src_test.go:87-89`

Old test asserted `serviceSourceManual`, new test asserts literal `"m"`. While `serviceSourceManual == "m"`, using the constant is better for maintainability -- if `serviceSourceManual` ever changes, this test would silently pass with the wrong value. Keep using the constant.

### N3. Inconsistent version assertion dropped

`ddtrace/tracer/tracer_test.go:2049,2060`

In the `universal` and `service/universal` sub-tests of `TestVersion`, the `assert.True(ok)` check was removed when switching from `sp.meta[ext.Version]` to `sp.meta.Get(ext.Version)`. The old code implicitly asserted presence (map lookup returns zero value for absent keys, so the Equal check served as an indirect presence check). The new code discards `ok` with `_`. This weakens the test -- a bug that fails to set version would now pass silently since `""` is a valid return for an absent key. Keep the `assert.True(ok)` assertion.

### N4. Minor: `h.buf.WriteString(",")` inconsistency

`ddtrace/tracer/writer.go:253`

Changed from `h.buf.WriteString(",")` (backtick) to `h.buf.WriteString(",")` (double-quote). This is functionally identical but introduces an unnecessary diff line. Not worth changing back, just noting the noise.

### N5. `TestSpanError` removed `nMeta` counting assertion

`ddtrace/tracer/span_test.go:983-2202`

The old test captured `nMeta := len(span.meta)` before `Finish` and then asserted `nMeta+4` after, validating that exactly 4 tags were added during finish (`_dd.p.dm`, `_dd.base_service`, `_dd.p.tid`, `_dd.svc_src`). The new test only asserts `Has(ext.ErrorMsg) == false`, which is weaker. The old assertion caught regressions where unexpected tags were added during finish. Consider restoring a count-based assertion using `span.meta.Count()`.

### N6. `dbSys` variable hoisted out of switch for no benefit

`ddtrace/tracer/spancontext.go:959`

```go
dbSys, _ := s.meta.Get(ext.DBSystem)
switch {
case s.hasMetaKeyLocked("aws_service"):
    ...
case dbSys == ext.DBSystemCassandra:
```

The `dbSys` lookup happens unconditionally even when the first `case` matches (AWS service). This is a minor efficiency concern -- the old code `s.meta[ext.DBSystem]` inside the case was lazily evaluated. In practice this is negligible, but it is a pattern change worth noting.
