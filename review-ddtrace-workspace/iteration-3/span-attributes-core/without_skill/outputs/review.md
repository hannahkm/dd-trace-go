# Code Review: PR #4538 - Promote span fields out of meta map into typed SpanAttributes struct

## Summary

This PR introduces `SpanAttributes` and `SpanMeta` types in `ddtrace/tracer/internal` to replace the plain `map[string]string` for `span.meta`. Promoted fields (env, version, language) are stored in a fixed-size array with a bitmask for presence tracking, while arbitrary tags remain in a flat map. A copy-on-write mechanism shares process-level attributes across spans, and an `Inline()`/`Finish()` step at span completion merges promoted attrs into the flat map for zero-allocation serialization.

---

## Blocking

### 1. PR description and code disagree on which fields are promoted

**span_attributes.go:139-148** and **span_meta.go:604-605**

The PR description repeatedly says the four promoted fields are `env`, `version`, `component`, and `span.kind`. However, the actual code promotes only three: `env`, `version`, and `language`. The `SpanAttributes` struct has `numAttrs = 3` and the `Defs` table lists `{"env", "version", "language"}`. Meanwhile `component` and `span.kind` are not promoted at all -- they remain in the flat map and are read via `sm.meta.Get(ext.Component)` / `sm.meta.Get(ext.SpanKind)` which just hits the flat map path.

This mismatch is confusing. The layout comment on `SpanAttributes` (line 163) says "1-byte setMask + 1-byte readOnly + 6B padding + [3]string (48B) = 56 bytes" which is consistent with 3 fields, but the description says 4 promoted fields and "72 bytes". The test `TestPromotedFieldsStorage` at **span_test.go:560-585** tests `ext.Component` and `ext.SpanKind` alongside `ext.Environment` and `ext.Version` -- those tests pass because `meta.Get()` works for flat-map keys too, but they do not actually verify that those fields are stored in the `SpanAttributes` struct. If `component` and `span.kind` were truly meant to be promoted, the implementation is incomplete.

**Recommendation:** Either update the PR description to accurately reflect that only `env`, `version`, and `language` are promoted, or add `component` and `span.kind` to the `AttrKey` constants and `Defs` table. This needs to be intentional -- the V1 encoder at **payload_v1.go:592-600** reads `component` and `spanKind` via `span.meta.Get(ext.Component)` which routes through the flat map, not the promoted path.

### 2. `deriveAWSPeerService` semantic change for S3 bucket lookup

**spancontext.go:935**

The old code checked `if bucket := sm[ext.S3BucketName]; bucket != ""` (checking that the bucket name is a non-empty string). The new code checks `if bucket, ok := sm.Get(ext.S3BucketName); ok` (checking only that the key is present). If a span has `ext.S3BucketName` set to an empty string `""`, the old code would fall through to the no-bucket path (`"s3.<region>.amazonaws.com"`), but the new code would produce `".s3.<region>.amazonaws.com"` (empty bucket prefix with a leading dot). This is a subtle behavioral change.

**Recommendation:** Restore the `bucket != ""` guard: `if bucket, ok := sm.Get(ext.S3BucketName); ok && bucket != ""`.

---

## Should Fix

### 3. `SpanAttributes.Set` is not nil-safe but all read methods are

**span_attributes.go:176-179**

`Set()` will panic on a nil receiver because it indexes into `a.vals[key]` without a nil check. Every other read method (`Val`, `Has`, `Get`, `Count`, `Unset`, `All`, `Reset`, `Clone`) is nil-safe. This asymmetry is surprising and could lead to panics if callers are not careful. The `ensureAttrsLocal()` in `SpanMeta` does guard against this, but `Set` being called on the raw `SpanAttributes` pointer (as it is in `buildSharedAttrs` and in tests) means someone could hit this.

**Recommendation:** Either add a nil-check with early allocation, or add a doc comment explicitly stating that `Set` panics on nil receiver. Given the pattern of all other methods being nil-safe, making `Set` nil-safe too would be more consistent.

### 4. `setMetaInit` no longer initializes the map, but `setMetaLocked` still calls it

**span.go:742-758** (diff lines around 1519-1536)

The old `setMetaInit` had `if s.meta == nil { s.meta = initMeta() }`. The new version removes this because `meta` is now a value type (`SpanMeta`), not a pointer. However, `setMetaInit` still calls `delete(s.metrics, key)` which can panic if `s.metrics` is nil. This is not new (the old code had the same issue), but since this PR is touching this function anyway, it would be good to guard it. More importantly, `setMetaInit` now calls `s.meta.Set(key, v)` in the default case, which for non-promoted keys will lazily allocate the internal map. This is fine but worth noting that the allocation profile changes -- previously the map was allocated upfront in `initMeta()`, now it is allocated on first non-promoted key write. For spans that only have promoted keys and metrics, this saves an allocation.

### 5. `civisibility_tslv.go` locking change - `Finish()` acquires lock after span is already finished

**civisibility_tslv.go:209-215** (diff lines 65-75)

The new code adds:
```go
func (e *ciVisibilityEvent) Finish(opts ...FinishOption) {
    e.span.Finish(opts...)
    e.span.mu.Lock()
    e.Content.Meta = e.span.meta.Map()
    e.Content.Metrics = e.span.metrics
    e.span.mu.Unlock()
}
```

This acquires the span lock after `Finish()` has already been called. After `Finish()`, the span may have already been flushed by the writer goroutine. While `meta.Map()` calls `Finish()` (which is idempotent due to the `inlined` atomic check), accessing `s.metrics` after the span has been potentially flushed could race with the writer's read. Additionally, `e.Content.Meta` and `e.Content.Metrics` are written here but may be read concurrently elsewhere without synchronization.

**Recommendation:** Verify that `ciVisibilityEvent.Content` is not accessed concurrently after `Finish()` is called, or consider capturing the map reference before calling `span.Finish()`.

### 6. Removal of `supportsLinks` field silently changes span link serialization behavior

**span.go:860-865** (diff lines 1556-1574)

The PR removes the `supportsLinks` field from `Span` and removes the `if s.supportsLinks { return }` early-return in `serializeSpanLinksInMeta()`. This means span links will now always be serialized as JSON in the `_dd.span_links` meta tag, even when the V1 protocol natively supports span links. The test `with_links_native` was removed from `TestSpanLinksInMeta`. This appears to be an intentional change (perhaps to always have the JSON fallback), but it means span links are now double-encoded: once natively in the V1 encoder and once as a JSON string in meta. This wastes payload space.

**Recommendation:** Clarify whether this is intentional. If V1 natively encodes span links, the JSON fallback in meta is redundant and increases payload size.

### 7. `IsPromotedKeyLen` is fragile and manually synced

**span_meta.go:817-831**

The `IsPromotedKeyLen` function uses a hardcoded switch on string lengths (3, 7, 8) corresponding to "env", "version", "language". While there is an `init()` check that verifies the `Defs` table matches, this only catches missing lengths -- it would not catch a new promoted key whose length collides with an existing non-promoted key, causing false positives in the fast path. The same lengths are duplicated in `Delete` (lines 791-796) with a comment explaining why inlining is avoided.

**Recommendation:** This is acceptable as-is given the `init()` guard, but consider generating these values or using a constant array to reduce the manual sync burden if more promoted keys are added in the future.

### 8. Test `TestPromotedFieldsStorage` does not actually verify promoted storage

**span_test.go:560-585**

This test claims to verify that "setting any of the four V1-promoted tags (env, version, component, span.kind) via SetTag stores the value in the dedicated SpanAttributes struct field inside meta." However, it only calls `span.meta.Get(tc.tag)` which works for both promoted attrs and flat-map entries. The test does not verify that the value is actually in `SpanAttributes` rather than the flat map. For `component` and `span.kind`, the values will be in the flat map, not in `SpanAttributes`, making the test description misleading.

**Recommendation:** Either update the test comment/name, or add assertions that directly check `span.meta.Attr(AttrEnv)` (for truly promoted fields) and verify that `component`/`span.kind` are in the flat map.

---

## Nits

### 9. Benchmark has a typo: reads `env` twice instead of `version`

**span_attributes_test.go:493**

In `BenchmarkSpanAttributesGet`, the "map" sub-benchmark reads `m["env"]` twice instead of reading `m["version"]` on the second call:
```go
s, ok = m["env"]
s, ok = m["version"]
s, ok = m["env"]       // should be m["language"] to match SpanAttributes sub-benchmark
s, ok = m["language"]
```

The SpanAttributes sub-benchmark reads 3 keys; the map sub-benchmark reads 4. This makes the comparison unfair.

### 10. `loadFactor` constant evaluates to 1 due to integer division

**span_meta.go:592**

```go
loadFactor = 4 / 3
```

In Go, integer division of `4 / 3` yields `1`, so `metaMapHint = expectedEntries * loadFactor = 5 * 1 = 5`. The comment says "~33% slack" which would imply `metaMapHint` should be ~6-7. This was copied from the old `initMeta()` in span.go which had the same issue.

**Recommendation:** Either accept that the hint is 5 (which is fine -- Go maps handle this) and update the comment, or use `expectedEntries * 4 / 3` to get the intended value of 6.

### 11. Comment on `SpanAttributes` layout is stale

**span_attributes.go:163**

The comment says "1-byte setMask + 1-byte readOnly + 6B padding + [3]string (48B) = 56 bytes" but the PR description says "72 bytes" and mentions "[4]string". The current code has `[numAttrs]string` where `numAttrs = 3`, so the size is indeed 56 bytes (with Go string headers being 16 bytes each: 3*16 = 48, plus 2 bytes for setMask/readOnly, plus 6 bytes padding = 56). The PR description is simply wrong about the size and array dimension.

### 12. Inconsistent use of `serviceSourceManual` vs literal `"m"` in tests

**srv_src_test.go:100,130-132**

In the test `ChildInheritsSrvSrcFromParent`, the assertion changed from `assert.Equal(t, serviceSourceManual, child.meta[ext.KeyServiceSource])` to `assert.Equal(t, "m", v)`. The constant `serviceSourceManual` should still be used here for readability and refactor safety. Similarly, `ChildWithExplicitServiceGetsSrvSrc` uses the literal `"m"` for the `Source` field in `ServiceOverride`.

### 13. `mocktracer` uses `unsafe.Pointer` for `sharedAttrs` parameter

**mockspan.go:19**

The `spanStart` linkname declaration now takes `sharedAttrs unsafe.Pointer` and passes `nil`. This works but is somewhat surprising -- the actual function signature takes `*traceinternal.SpanAttributes`. Using `unsafe.Pointer` here avoids importing the internal package, which is reasonable for a test helper using `go:linkname`, but a comment explaining this choice would be helpful.

### 14. `Range` skips promoted keys when `inlined=true` but callers may not expect this

**span_meta.go:713-723**

`Range` iterates over `sm.m` and skips promoted keys when `inlined=true`. This means after `Finish()`, `Range` excludes `env`, `version`, `language` from the iteration. The V1 encoder uses `Range` via `encodeMetaEntry` callback, where promoted keys should indeed be excluded (they are encoded separately). But other callers of `Range` (if any exist now or in the future) might not expect this filtering behavior. The `All()` method provides unfiltered iteration, but the distinction is subtle.

**Recommendation:** Add a doc comment on `Range` clarifying that it yields only non-promoted entries after `Finish()` and is intended for wire-format serialization. Callers needing all entries should use `All()`.
