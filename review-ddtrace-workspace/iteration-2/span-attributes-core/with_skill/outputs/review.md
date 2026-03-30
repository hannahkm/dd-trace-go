# Review: PR #4538 — Promote span fields out of meta map into typed SpanAttributes struct

## Summary

This PR introduces `SpanAttributes` (a compact fixed-size struct for promoted span fields) and `SpanMeta` (a replacement for `span.meta map[string]string` that combines a flat map with promoted attributes). The goal is to eliminate per-span allocations for promoted fields and reduce hash-map overhead on hot paths. The design uses copy-on-write sharing of process-level attributes across spans, and an `Inline()` / `Finish()` step that publishes promoted attrs into the flat map with an atomic release fence so serialization can proceed lock-free.

---

## Blocking

### 1. PR description / comments claim `component` and `span.kind` are promoted, but the code only promotes `env`, `version`, `language`

`span_attributes.go` defines exactly three promoted keys:

```go
AttrEnv      AttrKey = 0
AttrVersion  AttrKey = 1
AttrLanguage AttrKey = 2
numAttrs     AttrKey = 3
```

Yet the PR description says "stores the four V1-protocol promoted span fields (env, version, component, span.kind)", and multiple source comments repeat this claim:

- `span_meta.go:602` godoc: "Promoted attributes (env, version, component, span.kind, language) live in attrs"
- `span.go:139` comment: "Promoted attributes (env, version, component, span.kind) live in meta.attrs"
- `payload_v1.go:1167` comment: "env/version/language; component and span.kind live in the flat map"
- `span_test.go:2060` `TestPromotedFieldsStorage` tests `ext.Component` and `ext.SpanKind` as "V1-promoted tags", but these are not promoted -- they go through the normal flat map path.

This is confusing for anyone reading the code or reviewing the test. The stale documentation will cause future developers to assume `component`/`span.kind` are in the `SpanAttributes` struct when they are not. The test at `span_test.go:2060` passes by accident (because `SpanMeta.Get` falls through to the flat map for non-promoted keys), not because it is testing the promoted-field path it claims to test. Either the comments/test descriptions must be corrected to state that only `env`, `version`, and `language` are currently promoted, or `component` and `span.kind` should actually be added to `SpanAttributes`. This is a correctness-of-documentation issue that will mislead reviewers and future contributors.

### 2. `SpanAttributes.Set` is not nil-safe, unlike every other method

`span_attributes.go:176-179`:
```go
func (a *SpanAttributes) Set(key AttrKey, v string) {
    a.vals[key] = v
    a.setMask |= 1 << key
}
```

Every read method (`Val`, `Has`, `Get`, `Count`, `Unset`, `Reset`, `All`) checks `a == nil` and handles it gracefully. `Set` does not -- calling `Set` on a nil `*SpanAttributes` will panic. While the current call sites always ensure a non-nil receiver before calling `Set`, the inconsistency is a latent correctness bug. If a caller follows the pattern established by the read methods and assumes nil-safety, they will hit a nil pointer dereference. Either add a nil guard (allocating if nil, or documenting the panic contract), or document explicitly that `Set` panics on nil and why the asymmetry is intentional.

### 3. `deriveAWSPeerService` behavior change: empty string no longer treated as unset

`spancontext.go:914-926` changes `deriveAWSPeerService` from accepting `map[string]string` to `*SpanMeta`. The old code checked:
```go
service, region := sm[ext.AWSService], sm[ext.AWSRegion]
if service == "" || region == "" {
    return ""
}
```

The new code checks:
```go
service, ok := sm.Get(ext.AWSService)
if !ok {
    return ""
}
region, ok := sm.Get(ext.AWSRegion)
if !ok {
    return ""
}
```

These are semantically different. Previously, `service` being explicitly set to `""` caused an early return. Now, `service` set to `""` passes the `ok` check (because the key is present), and the function proceeds with an empty service string, potentially producing malformed peer service names like `.s3..amazonaws.com`. The same applies to `region`. The S3 bucket check also changed from `if bucket := sm[ext.S3BucketName]; bucket != ""` (value check) to `if bucket, ok := sm.Get(ext.S3BucketName); ok` (presence check), which similarly changes behavior for explicitly-empty values.

Either restore the empty-value guards (`service == "" || region == ""`) alongside the presence checks, or add a test that documents and validates the intended new behavior.

### 4. `ciVisibilityEvent.SetTag` drops `e.Content.Meta` synchronization

`civisibility_tslv.go:164`: The line `e.Content.Meta = e.span.meta` was removed from `SetTag`. The rebuilding now happens only in `Finish()`. If any CI Visibility consumer reads `e.Content.Meta` between a `SetTag` call and `Finish()`, they will see stale data. The comment in `Finish()` says "Rebuild Content.Meta once with the final span state" and acquires the span lock, which is correct for the finish path, but the removal from `SetTag` is only safe if there are no intermediate reads of `e.Content.Meta`. Verify this is the case or add a comment explaining why intermediate reads are impossible.

---

## Should Fix

### 5. Happy-path alignment in `abandonedspans.go`

`abandonedspans.go:85-89`: The existing pattern (unchanged by this PR but touched) has the happy path nested inside the `if` branch:

```go
if v, ok := s.meta.Get(ext.Component); ok {
    component = v
} else {
    component = "manual"
}
```

This should be flipped to early-assign the default and override:
```go
component = "manual"
if v, ok := s.meta.Get(ext.Component); ok {
    component = v
}
```

This is the single most frequent review comment in this repo.

### 6. `loadFactor = 4 / 3` is integer division, evaluates to 1

`span_meta.go:591-592`:
```go
loadFactor  = 4 / 3
metaMapHint = expectedEntries * loadFactor
```

Since these are untyped integer constants, `4 / 3 == 1`, so `metaMapHint == 5 * 1 == 5`. The comment says "provides ~33% slack" but the computation provides zero slack. This is identical to the pre-existing code in `span.go` (which had the same bug), so it is not a regression, but it is worth fixing now that the code is being moved to a new file. Use `metaMapHint = (expectedEntries * 4 + 2) / 3` or just `metaMapHint = 7` to get the intended ~33% slack.

### 7. Benchmark asymmetry in `BenchmarkSpanAttributesGet`

`span_attributes_test.go:481-498`: The `map` sub-benchmark performs 4 map lookups per iteration (`env`, `version`, `env` again, `language`) while the `SpanAttributes` sub-benchmark performs only 3. This makes the comparison unfair. The extra `m["env"]` lookup in the map benchmark should be removed to match the SpanAttributes benchmark, or the SpanAttributes benchmark should add a fourth lookup.

### 8. `for i := 0; i < b.N; i++` instead of `for range b.N`

`span_attributes_test.go:441-445, 451-456, 471-477, etc.`: Multiple benchmark loops use the pre-Go-1.22 style `for i := 0; i < b.N; i++`. Per the style guide for this repo, prefer `for range b.N`.

### 9. Test `TestPromotedFieldsStorage` misleadingly names non-promoted fields as promoted

`span_test.go:2057-2085`: As noted in blocking item #1, this test iterates over `ext.Component` and `ext.SpanKind` and calls them "V1-promoted tags" in the comment, but they are not promoted. The test passes because `Get` falls through to the flat map. If the intent is to test promoted field storage, test only `ext.Environment`, `ext.Version`, and `ext.Component`/`ext.SpanKind` should be tested separately as "non-promoted fields routed through the flat map". If the intent is to test that `Get` works for both promoted and non-promoted keys, rename the test to reflect that.

### 10. Removed test `with_links_native` without replacement

`span_test.go:1796-1293`: The `with_links_native` subtest was removed, and the `supportsLinks` field was removed from the `Span` struct. If span links are now always serialized in meta (JSON fallback), this is a behavioral change. The removed test verified that when native span link encoding was supported, the JSON fallback was skipped. If the v1 protocol now always handles span links natively (making the field unnecessary), this is fine, but there should be a test covering the new behavior to prevent regression.

### 11. `srv_src_test.go` changes `serviceSourceManual` to literal `"m"`

`srv_src_test.go:84,99,620,640`: Several assertions changed from using the constant `serviceSourceManual` to the literal string `"m"`. This is the opposite of what the repo conventions require (named constants over magic strings). If `serviceSourceManual` was intentionally changed or no longer applies, use whatever constant is appropriate; otherwise keep using `serviceSourceManual`.

---

## Nits

### 12. Comment says "four promoted fields" in `SpanAttributes` layout doc

`span_attributes.go:163`: The comment says `[4]string` but the actual array is `[3]string` (numAttrs=3). The PR description also says "four" in several places. Update for consistency.

### 13. `IsPromotedKeyLen` duplication in `Delete`

`span_meta.go:786-797`: The comment explains that the `switch len(key)` is intentionally duplicated from `IsPromotedKeyLen` to keep `Delete` inlineable. This is a good performance decision. However, the comment should reference a test or benchmark that validates the inlining budget claim, so future maintainers know to re-check if the function changes.

### 14. Godoc on `MarkReadOnly` says "readOnly (read-only)"

`span_attributes.go:214`: "marks this instance as readOnly (read-only)" -- the parenthetical is redundant. Just "marks this instance as read-only" suffices.

### 15. `String()` uses `fmt.Fprintf` in a hot-ish debug path

`span_meta.go:913-926`: The `String()` method uses `fmt.Fprintf(&b, "%s:%s", k, v)` which allocates. Since this is only called from `log.Debug` paths, it is not a blocking concern, but `b.WriteString(k); b.WriteByte(':'); b.WriteString(v)` would be allocation-free and consistent with the repo's preference for `strings.Builder` over `fmt.Sprintf` on non-trivial paths.

### 16. Missing blank line between third-party and Datadog imports

`span_meta.go:574-580`: The import block groups `iter`, `strings`, `sync/atomic` (stdlib) with `github.com/tinylib/msgp/msgp` (third-party) without a blank line separating them. Standard convention is three groups: stdlib, third-party, Datadog.
