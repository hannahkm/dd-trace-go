# Review: PR #4538 -- feat(ddtrace/tracer): promote span fields out of meta map into a typed SpanAttributes struct

**Author:** darccio (Dario Castane)
**Branch:** `dario.castane/apmlp-856/promote-redundant-span-fields` -> `main`
**Diff size:** +1553 / -461 across 30 files

## Summary

This PR introduces `SpanAttributes` (a fixed-size `[3]string` array with bitmask presence tracking) and `SpanMeta` (a wrapper combining the flat `map[string]string` with a `*SpanAttributes` pointer) to replace the plain `map[string]string` for `span.meta`. Three promoted fields (env, version, language) are stored in the typed struct; all other tags remain in the flat map. A copy-on-write mechanism shares process-level attrs across spans, and a `Finish()` / `Inline()` step copies promoted attrs into the flat map before serialization so that `EncodeMsg`/`Msgsize` can avoid allocations on the read path.

The change also adds a `scripts/msgp_span_meta_omitempty.go` helper to patch the generated `span_msgp.go` for omitempty support, comprehensive unit tests and benchmarks for the new types, and updates all callers throughout the tracer to use the new `SpanMeta` accessor methods.

## Reference files consulted

- style-and-idioms.md (always)
- concurrency.md (atomic fences, span field access during serialization, shared state)
- performance.md (hot-path changes: span creation, tag setting, serialization, encoding)

---

## Blocking

### 1. PR description says four promoted fields but code only promotes three

`span_attributes.go:139-148` declares `AttrEnv`, `AttrVersion`, `AttrLanguage` (numAttrs = 3). The PR description and several comments throughout the diff still reference "four V1-protocol promoted span fields (`env`, `version`, `component`, `span.kind`)" and "four promoted fields". The `Defs` table (line 246) only has three entries. The struct layout comment says "56 bytes" and "`[3]string`" but the PR description says "`[4]string`" and "72 bytes". This will confuse anyone reading the description or in-code comments that still reference four fields.

Additionally, `component` and `span.kind` are still read from `span.meta.Get(ext.Component)` / `span.meta.Get(ext.SpanKind)` in `payload_v1.go` (lines 1148-1149 in the diff), meaning they go through the flat map path, not the promoted fast path. The `TestPromotedFieldsStorage` test at `span_test.go:560-585` tests all four tags including `ext.Component` and `ext.SpanKind`, but those two will be stored in the flat map `m`, not in `SpanAttributes.vals`. The test passes (since `Get` checks both), but it validates the wrong invariant -- the test comment says "stores the value in the dedicated SpanAttributes struct field inside meta" which is incorrect for component and span.kind.

Either the description and comments need to be updated to reflect three promoted fields, or the code needs to actually promote all four. This is a correctness-of-documentation issue that will mislead reviewers and future maintainers.

### 2. `deriveAWSPeerService` changes behavior for S3 bucket with empty string value

`spancontext.go:924-926` (new code): The S3 bucket check changed from `if bucket := sm[ext.S3BucketName]; bucket != ""` (old: checks for non-empty value) to `if bucket, ok := sm.Get(ext.S3BucketName); ok` (new: checks for key presence). If a span has `ext.S3BucketName` explicitly set to an empty string, the old code falls through to `s3.<region>.amazonaws.com` while the new code would produce `.s3.<region>.amazonaws.com` (empty bucket prefix). This is a subtle behavioral change that could produce malformed peer service values when a bucket tag is explicitly set to empty.

### 3. `civisibility_tslv.go` `SetTag` drops the `Meta` sync but `Content.Meta` becomes stale

In `civisibility_tslv.go:160-162`, the old `SetTag` synced `e.Content.Meta = e.span.meta` on every call. The new code removes this (line 61 of the diff: `-e.Content.Meta = e.span.meta`), deferring Meta sync to `Finish()`. However, if any code reads `e.Content.Meta` between `SetTag` calls and before `Finish()`, it will see stale data. The `Finish` method now properly locks and calls `Map()`, but any intermediate reader of `Content.Meta` before `Finish` would see an empty or incomplete map. If CI Visibility serializes or inspects `Content` between tag setting and finish, this is a data loss bug.

### 4. `SpanAttributes.Set` does not check `readOnly` -- caller must ensure COW

`span_attributes.go:176-179`: `Set()` has no `readOnly` guard. If a caller accidentally calls `Set()` on a shared (read-only) instance without going through `SpanMeta.ensureAttrsLocal()`, it silently mutates the shared tracer-level instance, corrupting every span that shares it. The `SpanMeta` layer handles COW correctly, but `SpanAttributes.Set` is an exported method on a public type. A defensive panic (`if a.readOnly { panic("...") }`) would catch misuse immediately rather than allowing silent corruption.

---

## Should fix

### 5. `init()` function in `span_meta.go` -- avoid `init()` per repo convention

`span_meta.go:825-831` uses `func init()` to validate that `IsPromotedKeyLen` stays in sync with `Defs`. The style guide explicitly says "init() is very unpopular for go" in this repo. This could be replaced with a compile-time assertion (similar to the `[1]byte{}[AttrKey-N]` pattern already used in `span_attributes.go:153-157`) or a package-level `var _ = validatePromotedKeyLens()` call.

### 6. Benchmark `BenchmarkSpanAttributesGet` map sub-benchmark reads "env" twice

`span_attributes_test.go:491-494`: The map sub-benchmark reads `m["env"]` twice and `m["version"]` once, while the `SpanAttributes` sub-benchmark reads `AttrEnv`, `AttrVersion`, `AttrLanguage` (3 distinct keys). The asymmetric access pattern makes the comparison misleading. Fix: replace the duplicate `m["env"]` with `m["language"]` to match the SpanAttributes variant.

### 7. Benchmarks use old `for i := 0; i < b.N; i++` style

`span_attributes_test.go:441-445,453-456,473-477,482-486`: All four benchmark loops use `for i := 0; i < b.N; i++` instead of the Go 1.22+ `for range b.N` pattern that the style guide recommends and that other benchmarks in this PR already use (e.g., `BenchmarkMap` at line 556 uses `for range b.N`). Be consistent.

### 8. `loadFactor` integer division truncates to 1 -- `metaMapHint` equals `expectedEntries`

`span_meta.go:591-593`: `loadFactor = 4 / 3` is integer division, which evaluates to `1`, so `metaMapHint = expectedEntries * 1 = 5`. The comment says "provides ~33% slack" but actually provides zero slack. If the intent is to add slack, this should either use a different computation (e.g., `metaMapHint = expectedEntries + expectedEntries/3`) or `expectedEntries` should be bumped directly. Note: this was also present in the old `initMeta()` code, so it is a pre-existing issue being carried forward, but since the constants are being moved and redefined here it would be a good time to fix.

### 9. `SpanMeta.Count()` after `Finish()` double-counts promoted attrs

`span_meta.go:838-840`: `Count()` returns `len(sm.m) + sm.promotedAttrs.Count()`. After `Finish()` inlines promoted attrs into `sm.m`, the promoted keys exist in both `sm.m` and `sm.promotedAttrs`. This means `Count()` returns `len(sm.m) + N` where `N` promoted keys are already in `sm.m`. `SerializableCount()` handles this correctly (subtracts `promotedAttrs.Count()` when inlined), but the general `Count()` does not. If any code calls `Count()` after `Finish()` expecting the total number of distinct entries, it will get an inflated number. This may not be called post-Finish today, but it is an API contract bug waiting to happen.

### 10. Happy path alignment in `SpanMeta.DecodeMsg`

`span_meta.go:993-997`: The decode path uses a `if sm.m != nil` / `else` pattern to reuse or allocate the map. The happy path (allocation) is in the `else` block. Per the most-frequent review feedback, this should be flipped:

```go
if sm.m == nil {
    sm.m = make(map[string]string, header)
} else {
    clear(sm.m)
}
```

---

## Nits

### 11. Import alias consistency

The PR uses three different alias names for `ddtrace/tracer/internal`: `tinternal` (in test files), `traceinternal` (in production files), and the test for `internal` uses the default package name. Converging on a single alias would reduce cognitive load.

### 12. `fmt.Fprintf` in `SpanMeta.String()` on hot-ish path

`span_meta.go:923`: `fmt.Fprintf(&b, "%s:%s", k, v)` could be replaced with `b.WriteString(k); b.WriteByte(':'); b.WriteString(v)` to avoid the fmt overhead. This is only used for debug logging, so it is minor.

### 13. Removed `supportsLinks` field and test without explanation

The diff removes `supportsLinks` from the Span struct (`span.go:162-163`) and the `with_links_native` test case (`span_test.go:1796-1810`). The PR description does not mention this removal. Even if the field is no longer needed due to the serialization changes, the removal should be called out so reviewers can verify it is safe.

### 14. `serviceSourceManual` replaced with `"m"` in test expectations

`srv_src_test.go:600,619,649`: Several test assertions changed from comparing against `serviceSourceManual` constant to the literal string `"m"`. The test file still imports the constant elsewhere. Using the constant consistently is clearer and more resilient to value changes.
