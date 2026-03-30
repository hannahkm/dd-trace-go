# Review: PR #4538 — Promote span fields out of meta map into typed SpanAttributes struct

**PR:** https://github.com/DataDog/dd-trace-go/pull/4538
**Author:** darccio (Dario Castane)
**Branch:** `dario.castane/apmlp-856/promote-redundant-span-fields`

## Summary

This PR introduces `SpanAttributes` (a fixed-size array indexed by `AttrKey` constants with a presence bitmask) and `SpanMeta` (a wrapper combining a flat `map[string]string` with a `*SpanAttributes` for promoted keys). It replaces `span.meta map[string]string` with `SpanMeta`, routing promoted keys (`env`, `version`, `language`) through the typed struct and all other keys through the flat map. A copy-on-write mechanism shares process-level `SpanAttributes` across all spans, cloning only when a per-span override is needed. The `Finish()` method inlines promoted attrs into the flat map and sets an atomic flag so serialization can read the map lock-free.

---

## Blocking

### 1. PR description is stale / misleading about which fields are promoted

The PR body claims four promoted fields: `env`, `version`, `component`, `span.kind`. The actual implementation promotes only three: `env`, `version`, `language` (see `span_attributes.go:139-148`, `numAttrs = 3`). The PR description also references `sharedAttrsForMainSvc` being "pre-populated with `version` for main-service spans under `universalVersion=false`" and mentions `component` and `span.kind` COW triggers, but `component` and `span.kind` are not promoted in the code at all -- they remain in the flat map. The layout comment in `SpanAttributes` (`span_attributes.go:163`) says "72 bytes" but with `numAttrs=3` the struct is ~56 bytes (1+1+6 padding + 3*16 = 56), contradicting the comment.

This mismatch between description and implementation will confuse every reviewer. The description should be updated to match reality before merge.

### 2. `SpanAttributes.Set` is not nil-safe, unlike all read methods

`span_attributes.go:176-179`: `Set` dereferences `a` without a nil check, but every read method (`Val`, `Has`, `Get`, `Count`, `All`, `Unset`, `Reset`) is nil-safe. The `ensureAttrsLocal` method in `SpanMeta` (`span_meta.go:773-781`) covers the nil case before calling `Set`, but `SpanAttributes.Set` is exported and could be called directly. In `span_attributes_test.go` and `sampler_test.go`, test code calls `a.Set(...)` on non-nil instances only by construction. If anyone adds a test or consumer that calls `Set` on a nil `*SpanAttributes`, it will panic. Either add a nil guard or document the precondition in the godoc.

### 3. `ciVisibilityEvent.SetTag` reads `e.span.metrics` outside the span lock

`civisibility_tslv.go:163-164`: After calling `e.span.SetTag(key, value)` (which acquires and releases the lock internally), the next line reads `e.Content.Metrics = e.span.metrics` without holding `e.span.mu`. The old code had the same pattern for `e.Content.Meta = e.span.meta`, but that was a single pointer swap of a map reference. Now `e.Content.Meta` is not set here at all (deferred to `Finish`), but `e.span.metrics` is still read without synchronization. If another goroutine calls `SetTag` concurrently (setting a numeric metric), this is a data race on the map. The `Finish()` method correctly acquires the lock (`civisibility_tslv.go:213-216`), but `SetTag` does not. This pre-existed but the PR is already restructuring this code, so it should be fixed.

### 4. `s.meta.Finish()` is called after `setTraceTagsLocked` but before the span lock is released -- potential for write after Finish

In `spancontext.go:771-776`, `s.meta.Finish()` is called in `finishedOneLocked`. After `Finish()` is called, `sm.m` is supposed to be permanently read-only (per the doc comment on `SpanMeta.Finish`). However, looking at the partial flush path (`spancontext.go:785+`), `setTraceTagsLocked` is called on the first span in a chunk before `Finish()`. But what about the case where a span finishes, `Finish()` is called on its meta, and then later during partial flush of the trace, `setTraceTagsLocked` is called on the same span (which is the first span in a new chunk)? The code at line 757-763 calls `setTraceTagsLocked(s)` for `s == t.spans[0]`, but `s` here is the span that just finished. If that span is later reused as `t.spans[0]` in a partial flush chunk, the trace tags would be set on an already-`Finish()`ed meta. The `setMetaLocked` call would write to a meta whose `inlined` flag is already true, meaning writes go to the flat map but `SerializableCount` and `Range` will skip promoted keys that are now in `sm.m`. This needs careful analysis to confirm it cannot happen. At minimum, add a comment explaining why this ordering is safe.

---

## Should Fix

### 5. Happy path not left-aligned in `abandonedspans.go`

`abandonedspans.go:87-90` (unchanged but visible in diff context): The `if v, ok := s.meta.Get(ext.Component); ok { ... } else { component = "manual" }` pattern wraps the happy path in the `if` block instead of using an early assignment. This should be:
```go
component = "manual"
if v, ok := s.meta.Get(ext.Component); ok {
    component = v
}
```
This is the most common review comment pattern. The PR is touching this line (changing map access to `.Get()`), so it's a good time to fix the style.

### 6. Duplicated `mkSpan` helpers in sampler_test.go

`sampler_test.go`: The `mkSpan` function is duplicated verbatim in at least five test functions (`TestPrioritySamplerRampCooldownNoReset`, `TestPrioritySamplerRampUp`, `TestPrioritySamplerRampDown`, `TestPrioritySamplerRampConverges`, `TestPrioritySamplerRampDefaultRate`). Each creates a `SpanAttributes`, sets `AttrEnv`, and returns a `Span`. This should be extracted to a single package-level test helper. The pattern of `a := new(tinternal.SpanAttributes); a.Set(tinternal.AttrEnv, env); return &Span{service: svc, meta: tinternal.NewSpanMeta(a)}` is repeated identically.

### 7. Benchmark uses wrong key in `BenchmarkSpanAttributesGet`

`span_attributes_test.go:494`: The map benchmark reads `m["env"]` twice and `m["version"]` once, but skips `m["language"]` entirely (3 reads but one is duplicated: `s, ok = m["env"]` appears on lines 492 and 494). The `SpanAttributes` benchmark correctly reads all three keys. This makes the benchmark comparison unfair. Should be `m["language"]` on the third read.

### 8. `for i := 0; i < b.N; i++` should be `for range b.N`

`span_attributes_test.go:441,453,473,493`: Multiple benchmarks use the old-style `for i := 0; i < b.N; i++` loop instead of `for range b.N` (Go 1.22+). Other benchmarks in the same file already use `for range b.N` (line 556). The style guide says to prefer the modern form.

### 9. Magic string `"m"` for service source in test

`srv_src_test.go:619-620`: The test value `"m"` is used as the service source string, but the old code used `serviceSourceManual`. The assertion `assert.Equal(t, "m", v)` at line 619 replaces `assert.Equal(t, serviceSourceManual, child.meta[ext.KeyServiceSource])`. If `serviceSourceManual` is the constant `"m"`, then this change loses the semantic reference to the named constant. Use the constant in the test for clarity.

### 10. Magic numbers in `Delete` length switch

`span_meta.go:791-796`: The `Delete` method duplicates the `IsPromotedKeyLen` switch with magic numbers `3, 7, 8`. The comment explains this is intentional for inlining budget reasons, which is a good explanation. However, this creates a maintenance hazard if promoted keys are added or renamed. Consider adding a compile-time assertion or `init()` check that validates the lengths in `Delete` match `IsPromotedKeyLen`, similar to the existing `init()` check for `IsPromotedKeyLen` vs `Defs`.

### 11. `TestPromotedFieldsStorage` tests `component` and `span.kind` as promoted, but they are not

`span_test.go:2060-2085`: This test iterates over `ext.Environment`, `ext.Version`, `ext.Component`, and `ext.SpanKind`, and calls `span.meta.Get(tc.tag)` to verify they are stored. However, `component` and `span.kind` are NOT promoted attributes -- they are stored in the flat map, not in `SpanAttributes`. The test passes because `.Get()` checks both the attrs struct and the flat map, but the test name and doc comment claim these are "V1-promoted tags" stored in "the dedicated SpanAttributes struct field inside meta", which is incorrect for `component` and `span.kind`. The test should be renamed and the doc comment corrected, or the test should be split into two groups (promoted vs. flat-map tags).

---

## Nits

### 12. Layout comment in `SpanAttributes` is stale

`span_attributes.go:163`: "Layout: 1-byte setMask + 1-byte readOnly + 6B padding + [3]string (48B) = 56 bytes." The field list says `[numAttrs]string` where `numAttrs=3`, so 3 * 16 = 48 bytes for the array, plus 2 bytes for the flags, plus 6 bytes padding = 56 bytes total. The comment says "56 bytes" which is correct, but the PR description says "72 bytes". The PR description should be updated.

### 13. Import alias inconsistency

The codebase introduces two different aliases for `ddtrace/tracer/internal`:
- `tinternal` in `sampler_test.go`, `span_test.go`, `stats_test.go`, `transport_test.go`, `writer_test.go`
- `traceinternal` in `span.go`, `spancontext.go`, `tracer.go`

Pick one and use it consistently.

### 14. Unnecessary blank line removal in `deriveAWSPeerService`

`spancontext.go:921,930,934`: The PR removes blank lines between `case` blocks in the `switch` statement inside `deriveAWSPeerService`. This is a minor style change unrelated to the feature -- the blank lines between cases were valid formatting. Not blocking, but unrelated formatting changes in a large PR add noise.

### 15. Comment refers to non-existent `val()`

`payload_v1.go:594-595` and `sampler.go:277-278`: Comments say "val() is used" but the code uses `.Env()`, `.Version()`, `.Get()` -- there is no `val()` method. These should say something like "The value is used (ok is discarded)" or simply explain the semantics directly.

### 16. `loadFactor` constant evaluates to 1 due to integer division

`span_meta.go:591-592`: `loadFactor = 4 / 3` evaluates to `1` in integer arithmetic, making `metaMapHint = expectedEntries * 1 = 5`. The comment says "~33% slack" but no slack is actually applied. This likely pre-existed (the same constants are moved from `span.go`'s `initMeta`), but worth noting since the PR is the one defining these constants in the new location.
