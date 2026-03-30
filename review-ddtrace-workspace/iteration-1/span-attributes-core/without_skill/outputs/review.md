# Code Review: PR #4538 -- Promote span fields out of meta map into typed SpanAttributes struct

## Blocking

### 1. `SpanAttributes.Set()` is not nil-safe, unlike all other methods
**File:** `ddtrace/tracer/internal/span_attributes.go:176-179`

Every read method on `*SpanAttributes` (`Val`, `Has`, `Get`, `Unset`, `Count`, `Reset`, `All`) has a nil-receiver guard, and the doc at line 174 explicitly states "All read methods are nil-safe so callers holding a `*SpanAttributes` don't need nil guards." However, `Set()` has no nil check and will panic on a nil receiver. Since `SpanMeta.ensureAttrsLocal()` allocates before calling `Set`, callers currently reach `Set` through a non-nil pointer. But nothing prevents a direct call like `var a *SpanAttributes; a.Set(AttrEnv, "prod")` -- and the asymmetry with every other method is a latent bug. Either add a nil guard (allocating a new instance), or document that `Set` requires a non-nil receiver and add a compile-time or runtime assertion.

### 2. `SpanMeta.Count()` double-counts after `Finish()` is called
**File:** `ddtrace/tracer/internal/span_meta.go:338-340`

```go
func (sm *SpanMeta) Count() int {
    return len(sm.m) + sm.promotedAttrs.Count()
}
```

After `Finish()` inlines promoted attrs into `sm.m`, both `len(sm.m)` and `sm.promotedAttrs.Count()` include them. `Count()` will over-report by `promotedAttrs.Count()`. While `Count()` is currently only called in tests before `Finish()`, the method is exported and its doc says "total number of distinct entries" with no caveat about timing. Either gate on `inlined.Load()` (as `SerializableCount` and `IsZero` do), or document that `Count()` must not be called after `Finish()`.

### 3. `deriveAWSPeerService` changes behavior for S3 bucket name check
**File:** `ddtrace/tracer/spancontext.go:~925` (new line, `case "s3":` branch)

Old code:
```go
if bucket := sm[ext.S3BucketName]; bucket != "" {
```

New code:
```go
if bucket, ok := sm.Get(ext.S3BucketName); ok {
```

The old code checked `bucket != ""` (empty bucket name was treated as absent). The new code checks only `ok` (presence). If a span has `ext.S3BucketName` explicitly set to `""`, the new code will produce a malformed hostname like `.s3.us-east-1.amazonaws.com`. This is a subtle behavioral regression. Either keep the `bucket != ""` guard alongside `ok`, or add a `&& bucket != ""` to match the old semantics.

### 4. `unsafe.Pointer` in mocktracer `go:linkname` signature
**File:** `ddtrace/mocktracer/mockspan.go:19`

```go
func spanStart(operationName string, sharedAttrs unsafe.Pointer, options ...tracer.StartSpanOption) *tracer.Span
```

The actual `spanStart` function takes `*traceinternal.SpanAttributes`, but the mock declares it as `unsafe.Pointer`. While this works at the ABI level (both are pointer-sized), it circumvents type safety and future refactors. If the `traceinternal` package is importable, use the real type. If not importable from the mock, consider exporting a thin wrapper that the mock can call instead. At minimum, add a comment explaining why `unsafe.Pointer` is used and link it to the real signature.

---

## Should Fix

### 5. `loadFactor = 4 / 3` evaluates to `1` due to integer division
**File:** `ddtrace/tracer/internal/span_meta.go:91-92`

```go
loadFactor  = 4 / 3      // Go integer division => 1
metaMapHint = expectedEntries * loadFactor  // => 5 * 1 = 5
```

The comment says this provides "~33% slack", but `4 / 3` in Go is integer division and evaluates to `1`, not `1.33`. So `metaMapHint` is `5`, providing zero slack. This was the same bug in the old `initMeta()` code, but the PR moved it without fixing it. To get the intended behavior, compute `(expectedEntries * 4) / 3` or use a literal `7`.

### 6. PR description and code comments mention `component` and `span.kind` as promoted attributes, but they are not
**File:** `ddtrace/tracer/internal/span_attributes.go`, `ddtrace/tracer/internal/span_meta.go:602`, `ddtrace/tracer/span.go:139-141`

The PR description says the four promoted fields are `env`, `version`, `component`, `span.kind`. Several code comments echo this (e.g., span_meta.go line 602: "Promoted attributes (env, version, component, span.kind, language)"). But `SpanAttributes` only defines three: `AttrEnv`, `AttrVersion`, `AttrLanguage`. The `AttrKeyForTag` tests explicitly assert `component` and `span.kind` return `AttrUnknown`. The stale comments will confuse future readers and reviewers. Update all comments to list the actual promoted set: `env`, `version`, `language`.

### 7. Test `TestPromotedFieldsStorage` tests `ext.Component` and `ext.SpanKind` as "promoted" but they are not
**File:** `ddtrace/tracer/span_test.go:2060-2085`

The test is titled "TestPromotedFieldsStorage" and its doc says "setting any of the four V1-promoted tags (env, version, component, span.kind) via SetTag stores the value in the dedicated SpanAttributes struct field." But `component` and `span.kind` are stored in the flat map, not in `SpanAttributes`. The test passes because `span.meta.Get()` searches both the promoted attrs and the flat map, so it will find the value regardless. This test does not actually verify that promoted storage works differently from flat-map storage. The test should be updated to verify only the actual promoted keys (`env`, `version`) or restructured to test that `component`/`span.kind` go to the flat map.

### 8. CI visibility `SetTag` no longer updates `Content.Meta` per-call
**File:** `ddtrace/tracer/civisibility_tslv.go:164-166`

Old code updated `e.Content.Meta = e.span.meta` after every `SetTag` call. New code removes that line entirely from `SetTag` and defers the assignment to `Finish()`. If any CI visibility code reads `e.Content.Meta` between `SetTag` calls (before `Finish`), it will see stale data. The `Finish()` path now properly acquires the lock and snapshots the final state, which is correct, but verify that no CI visibility consumer reads `Content.Meta` before `Finish()`.

### 9. Removal of `supportsLinks` field changes span link serialization behavior
**File:** `ddtrace/tracer/span.go:849-860`

The `supportsLinks` field and its guard in `serializeSpanLinksInMeta()` were removed. Previously, when the V1 protocol was active (`supportsLinks = true`), span links were NOT serialized into meta as JSON (they were encoded natively). Now, span links are ALWAYS serialized into meta as JSON, even when V1 encoding will also encode them natively. This means V1-encoded spans will have span links in both the native `span_links` field AND in `meta["_dd.span_links"]` as JSON, doubling the payload size for spans with links. The corresponding test `with_links_native` was also deleted instead of being updated.

---

## Nits

### 10. `BenchmarkSpanAttributesGet` map sub-benchmark reads `m["env"]` twice
**File:** `ddtrace/tracer/internal/span_attributes_test.go:490-494`

```go
s, ok = m["env"]
s, ok = m["version"]
s, ok = m["env"]      // duplicate -- should be m["language"]
s, ok = m["language"]
```

The map benchmark performs 4 reads (with `m["env"]` duplicated) while the `SpanAttributes` benchmark performs 3 reads. This makes the comparison unfair. Change the duplicate `m["env"]` to something else or align the number of reads.

### 11. `deriveAWSPeerService` also changes semantics for `service` and `region`
**File:** `ddtrace/tracer/spancontext.go:914-921`

Old code checked `service == "" || region == ""` (treated empty-string as absent). New code checks `!ok` (only checks presence). This is consistent with the change for S3BucketName (item 3 above) but affects the main function entry. If `ext.AWSService` is set to `""`, the old code would return `""` (no peer service) but the new code continues processing, potentially generating `".us-east-1.amazonaws.com"`. This is a minor behavioral change that should be documented or guarded.

### 12. `ChildInheritsSrvSrcFromParent` test asserts `"m"` instead of `serviceSourceManual`
**File:** `ddtrace/tracer/srv_src_test.go:86-87`

```go
v, _ := child.meta.Get(ext.KeyServiceSource)
assert.Equal(t, "m", v)
```

The old code used the named constant `serviceSourceManual`. Using the literal `"m"` here makes the test more fragile and less readable. Keep using `serviceSourceManual` for consistency with other tests in the same file.

### 13. Minor: `SpanAttributes` struct size comment says `[4]string` in PR description
**File:** PR description

The PR description says "typed `[4]string` array" and "Total size: 72 bytes" but the code uses `[3]string` (numAttrs=3) with a total of 56 bytes. The description should be updated to match the code.

### 14. `SpanMeta.String()` iterates via `All()` which does not respect `inlined` dedup
**File:** `ddtrace/tracer/internal/span_meta.go:413-426`

`All()` yields `sm.m` entries first, then promoted attrs. After `Finish()`, `sm.m` already contains the promoted keys, and `All()` checks `sm.inlined.Load()` to skip the attrs loop. This works correctly. However, if `String()` is called before `Finish()` and `sm.m` happens to contain a promoted key (which should not happen by design), it would be yielded twice. This is a minor concern since the invariant "promoted keys never appear in sm.m before Finish()" is maintained by the write path.

### 15. Inconsistent `assert.Equal` argument order in updated tests
**File:** `ddtrace/tracer/tracer_test.go:2808-2809`

```go
assert.Equal(t, v, "yes")
assert.Equal(t, v, "partial")
```

The `testify` convention is `assert.Equal(t, expected, actual)`. Here the arguments are swapped -- `v` (actual) is the second arg and `"yes"` (expected) is the third. This won't fail, but the error messages will be confusing on failure ("expected: `<actual>`, got: `<expected>`").
