# PR #4538 Review: Promote span fields out of meta map into typed SpanAttributes struct

## Blocking

### 1. PR description claims 4 promoted fields, code only promotes 3 -- `component` and `span.kind` are NOT promoted

**Files:** `ddtrace/tracer/internal/span_attributes.go:16-27`, PR description

The PR description says: "SpanAttributes -- a compact, fixed-size struct that stores the four V1-protocol promoted span fields (env, version, component, span.kind)". But the actual code defines only 3 promoted attributes:

```go
AttrEnv      AttrKey = 0
AttrVersion  AttrKey = 1
AttrLanguage AttrKey = 2
numAttrs     AttrKey = 3
```

There is no `AttrComponent` or `AttrSpanKind`. `component` and `span.kind` remain in the flat map `m`. The `AttrLanguage` attribute is present but never mentioned in the PR description. This is a significant documentation-vs-code mismatch that will confuse reviewers and future maintainers. The struct layout comment says "[3]string (48B) = 56 bytes" -- consistent with 3 fields, not 4. Either update the PR description to accurately reflect the implementation (3 promoted fields: env, version, language), or add the missing `AttrComponent`/`AttrSpanKind` constants if they were intended.

### 2. `TestPromotedFieldsStorage` test comment is misleading about what it actually tests

**File:** `ddtrace/tracer/span_test.go` (new test, around diff line 2056-2085)

The test says "verifies that setting any of the four V1-promoted tags (env, version, component, span.kind) via SetTag stores the value in the dedicated SpanAttributes struct field inside meta. Promoted fields no longer appear in the meta.m map." However, `component` and `span.kind` are NOT promoted -- they will be stored in the flat map, not in `SpanAttributes`. The test still passes because `SpanMeta.Get()` checks both `attrs` and `m`, but the assertion "Promoted fields no longer appear in meta.m" is false for `component` and `span.kind`. This test gives false confidence about the promoted-field claim.

### 3. `SpanAttributes.Set` panics on nil receiver

**File:** `ddtrace/tracer/internal/span_attributes.go:46-49`

```go
func (a *SpanAttributes) Set(key AttrKey, v string) {
    a.vals[key] = v
    a.setMask |= 1 << key
}
```

Unlike `Unset`, `Val`, `Has`, `Get`, `Count`, and `Reset`, the `Set` method is NOT nil-safe. The code says "All read methods are nil-safe" but `Set` is the only write method that can panic if called on a nil pointer. Since `SpanMeta.ensureAttrsLocal()` guards against this in practice, the risk is limited to direct callers of `SpanAttributes.Set()`. The `buildSharedAttrs` function in `tracer.go` calls `base.Set(...)` and `mainSvc.Set(...)` which are stack-allocated, so those are safe. However, this is an asymmetry in the nil-safety contract that should either be documented (explicitly noting Set requires non-nil) or handled with a nil guard.

## Should Fix

### 4. `civisibility_tslv.go:Finish()` takes `span.mu.Lock()` AFTER `span.Finish()` -- possible double-lock with trace lock

**File:** `ddtrace/tracer/civisibility_tslv.go:209-216`

```go
func (e *ciVisibilityEvent) Finish(opts ...FinishOption) {
    e.span.Finish(opts...)
    e.span.mu.Lock()
    e.Content.Meta = e.span.meta.Map()
    e.Content.Metrics = e.span.metrics
    e.span.mu.Unlock()
}
```

After `span.Finish()` returns, the span may have already been handed off to the trace writer. Taking `span.mu.Lock()` here to read `meta.Map()` and `metrics` could conflict with the writer goroutine's access. Additionally, `meta.Map()` calls `Finish()` which sets the `inlined` atomic bool -- but `meta.Finish()` was already called in `trace.finishedOneLocked`. This is a redundant `Finish()` call. The `meta.Finish()` idempotency check (`if sm.inlined.Load() { return }`) means it won't double-inline, but the locking interaction after span submission is concerning. Also, the old code set `e.Content.Meta = e.span.meta` in `SetTag` -- the new code removed that line and only sets it in `Finish()`, meaning CI visibility events that read `Content.Meta` between `SetTag` and `Finish` would see stale data.

### 5. `Count()` double-counts after `Finish()` / `Inline()`

**File:** `ddtrace/tracer/internal/span_meta.go:104-106`

```go
func (sm *SpanMeta) Count() int {
    return len(sm.m) + sm.promotedAttrs.Count()
}
```

After `Finish()` is called, promoted attrs are copied INTO `sm.m`, so `len(sm.m)` already includes the promoted keys. But `promotedAttrs.Count()` still returns the number of promoted fields (since `promotedAttrs` is not cleared). So `Count()` will return `len(sm.m) + promotedAttrs.Count()` which double-counts promoted entries. For example, if you have 2 flat-map entries and 3 promoted attrs, after `Finish()` `sm.m` has 5 entries and `Count()` returns 5+3=8 instead of 5.

This may not cause issues if `Count()` is never called after `Finish()`, but it is called in tests (e.g., `span_test.go` `TestSpanErrorNil`) and is a public API on an exported type. The `SerializableCount()` method correctly handles the post-inline case by subtracting `promotedAttrs.Count()` when inlined, but `Count()` does not.

### 6. `IsPromotedKeyLen` length check is a fragile optimization that could miss future promoted keys

**File:** `ddtrace/tracer/internal/span_meta.go:83-90`

```go
func IsPromotedKeyLen(n int) bool {
    switch n {
    case 3, 7, 8:
        return true
    }
    return false
}
```

The `init()` check validates that all `Defs` entries have lengths that match `IsPromotedKeyLen`, but it does NOT check the reverse: that all lengths in the switch are covered by `Defs`. If a promoted key is removed but its length remains in the switch, the check still passes but causes unnecessary slow-path calls. More importantly, the hardcoded length values in `Delete()` are intentionally duplicated rather than calling `IsPromotedKeyLen` to stay under the inlining budget. This means there are TWO places where promoted key lengths must be kept in sync -- the `Delete` switch and `IsPromotedKeyLen`. The comment in `Delete` explains the duplication, which is appreciated, but this is still a maintenance hazard.

### 7. `deriveAWSPeerService` behavior change: now returns "" for empty service/region strings

**File:** `ddtrace/tracer/spancontext.go:914-926`

The old code checked `service == "" || region == ""`. The new code checks `!ok` from `sm.Get()`. But after `Finish()` (which is called before peer service calculation in `finishedOneLocked`), promoted attrs are in `sm.m`, and `sm.Get()` for non-promoted keys checks only `sm.m`. The behavior change is: if `ext.AWSService` is set to `""` explicitly, old code returns `""` (because `service == ""`), new code also returns `""` (because `ok` is true but then the `strings.ToLower` switch won't match). However, the `S3BucketName` check changed from `bucket != ""` to `ok` -- meaning an explicitly empty bucket name will now produce `".s3.region.amazonaws.com"` instead of falling through to `s3.region.amazonaws.com`. This is a subtle behavioral change.

### 8. `srv_src_test.go:ChildInheritsSrvSrcFromParent` asserts `"m"` instead of `serviceSourceManual`

**File:** `ddtrace/tracer/srv_src_test.go:87-88`

```go
v, _ := child.meta.Get(ext.KeyServiceSource)
assert.Equal(t, "m", v)
```

The old test asserted `serviceSourceManual` (the constant). The new test hardcodes `"m"`. If `serviceSourceManual` ever changes from `"m"`, this test would silently pass with the wrong expectation. Use the constant.

## Nits

### 9. `BenchmarkSpanAttributesGet` map sub-benchmark reads "env" twice instead of all 3 keys

**File:** `ddtrace/tracer/internal/span_attributes_test.go:483-498`

```go
b.Run("map", func(b *testing.B) {
    m := map[string]string{
        "env":      "prod",
        "version":  "1.2.3",
        "language": "go",
    }
    ...
    for i := 0; i < b.N; i++ {
        s, ok = m["env"]
        s, ok = m["version"]
        s, ok = m["env"]       // <-- should be m["language"]
        s, ok = m["language"]
    }
```

The map benchmark reads "env" twice and then "language", performing 4 lookups. The SpanAttributes benchmark reads 3 keys. This skews the comparison. Change the duplicate `m["env"]` to remove it, or add a 4th SpanAttributes read.

### 10. Struct layout comment is stale

**File:** `ddtrace/tracer/internal/span_attributes.go:29-33`

```go
// Layout: 1-byte setMask + 1-byte readOnly + 6B padding + [3]string (48B) = 56 bytes.
```

The PR description says "Total size: 72 bytes" (referencing the old 4-field version with `[4]string`). The code says 56 bytes. One of these is wrong. Also, `[3]string` on 64-bit is actually `3 * 16 = 48` bytes for the string headers, plus `1 + 1 + 6 = 8` bytes padding, totaling 56 bytes. The code comment matches the implementation, but the PR description's 72-byte claim is outdated.

### 11. `loadFactor` integer division truncates to 1

**File:** `ddtrace/tracer/internal/span_meta.go:58-59`

```go
loadFactor  = 4 / 3
metaMapHint = expectedEntries * loadFactor
```

`4 / 3` in Go integer arithmetic is `1`, so `metaMapHint = 5 * 1 = 5`. The comment says "~33% slack" but there is zero slack. If the intent is to provide headroom, use `expectedEntries * 4 / 3` (which gives 6) or define `metaMapHint` directly as 7.

### 12. `Removed supportsLinks` field without explanation in PR description

**File:** `ddtrace/tracer/span.go:162-163` (removal), `ddtrace/tracer/span_test.go:1796-1810` (removed test)

The `supportsLinks` field on `Span` and its associated test (`with_links_native`) were removed. The PR description does not mention this removal. The `serializeSpanLinksInMeta` function no longer checks `s.supportsLinks` before serializing, meaning span links will now always be serialized in meta as JSON even when the V1 protocol supports native span links. This seems like a separate behavioral change that should be called out.

### 13. Minor: `s.meta.String()` format uses `%s:%s` not `%s: %s`

**File:** `ddtrace/tracer/internal/span_meta.go:79-92`

The `String()` method uses `fmt.Fprintf(&b, "%s:%s", k, v)` which matches the Go `fmt.Sprint(map[string]string{...})` format. This is fine but worth noting it produces `map[key:value]` without spaces after the colon.

### 14. `Normalize()` is test-only but exported

**File:** `ddtrace/tracer/internal/span_meta.go:16-23`

The `Normalize()` method comment says "Intended for test helpers" but it's an exported method on an exported type. Consider making it unexported or moving it to a test file with `//go:linkname` if it's truly test-only.
