# Code Review: PR #4528 — fix(internal/orchestrion): fix span parenting with GLS

**PR URL:** https://github.com/DataDog/dd-trace-go/pull/4528
**Status:** MERGED (2026-03-20)
**Approvals:** kakkoyun, RomainMuller, rarguelloF, mtoffl01

---

## Summary

This PR fixes three independent but related bugs in Orchestrion's GLS-based span propagation system. All three bugs cause incorrect span parent assignment when instrumented Go code uses custom context types or calls `context.Background()`. The changes touch the core GLS/context machinery in `internal/orchestrion/context.go`, the `//dd:span` injection template in `ddtrace/tracer/orchestrion.yml`, and updates integration tests for both `graphql-go` and `99designs/gqlgen` to reflect the corrected span hierarchy.

---

## Core Logic Changes

### Bug 1: `context.Background()` inherits active GLS span (`ddtrace/tracer/context.go`)

**Change:** In `SpanFromContext`, the nil *Span check was refactored from `return s, s != nil` to an explicit `if s == nil { return nil, false }` guard.

**Assessment:** The refactor is functionally equivalent but cleaner. The comment is accurate. However, the PR description claims there is a `context.Background()` sentinel early-return fix in `SpanFromContext`, but no such early-return is visible in the diff — the fix is only the `nil *Span` guard, which is a different (though related) concern. Looking at the existing code, `WrapContext(ctx).Value(...)` is still called even for `context.Background()`, so the GLS fallback can still activate for a Background context. The PR description's explanation of "Bug 1" does not seem fully consistent with the actual diff — the actual change prevents a nil-span type assertion panic, not the GLS-fallback-on-Background problem as described. This deserves a closer look at whether Bug 1 is fully addressed or only partially.

**Minor code style note:** The refactoring of `contextWithPropagatedLLMSpan` to remove the `newCtx` intermediate variable is a correct cleanup with no behavioral change.

### Bug 2: GLS overrides explicitly-propagated context (`internal/orchestrion/context.go`)

**Change:** In `glsContext.Value`, the lookup order was reversed: the explicit context chain is now consulted first, and GLS is only consulted as a fallback if the chain returns `nil`.

```go
// Before:
if val := getDDContextStack().Peek(key); val != nil {
    return val
}
return g.Context.Value(key)

// After:
if val := g.Context.Value(key); val != nil {
    return val
}
if val := getDDContextStack().Peek(key); val != nil {
    return val
}
return nil
```

**Assessment:** This is the most impactful fix and the logic is correct. GLS is designed as an implicit fallback for call sites that lack a `context.Context` parameter; it must not override explicitly-propagated contexts. The new order correctly prioritizes the explicit chain over GLS.

One subtle behavioral change introduced here: the old code called `g.Context.Value(key)` at the end (returning its result whether nil or not), while the new code returns `nil` unconditionally if both the context chain and GLS return nil. This is correct because if neither source has the key, `nil` is the right answer — but it's worth noting that this removes the final "fallthrough" to the wrapped context's own nil return, which is equivalent since both return nil for a missing key.

The comment added above the new lookup is clear and well-written.

**Coverage concern:** Codecov reports 0% coverage on the new lines in `internal/orchestrion/context.go`. The `TestSpanHierarchy` tests added for `graphql-go` and `gqlgen` exercise these paths indirectly, but the coverage tool may not be tracking integration tests. Unit tests directly exercising `glsContext.Value` with both a context-chain value and a GLS value would strengthen confidence.

### Bug 3: `*CustomContext` not recognized as context source in `//dd:span` (`ddtrace/tracer/orchestrion.yml`)

**Change:** The `//dd:span` injection template was extended to handle a function argument that *implements* `context.Context` (via `ArgumentThatImplements "context.Context"`) in addition to exact `context.Context` type matches.

**Assessment:** The template logic is correct in intent. Two sub-cases are handled:

1. When the implementing argument is named `ctx` — a rename to `__dd_span_ctx` is needed to avoid shadowing, and a nil-guard is added.
2. When the implementing argument has another name — it is assigned to a temporary `__dd_ctxImpl`, then used to initialize `var ctx context.Context` with a nil-guard.

**Potential issue — name collision with `__dd_ctxImpl`:** If a function has a parameter already named `__dd_ctxImpl`, this injected code will produce a compile error. This is a degenerate case, but the existing Orchestrion template conventions should document or handle reserved-name conflicts. Consider using a more unique prefix (e.g., `__dd_orch_ctxImpl`) though this is low priority given that `__dd_` prefix is already Orchestrion-reserved.

**Potential issue — multiple context-implementing arguments:** `ArgumentThatImplements` presumably returns the first matching argument. If a function has two arguments that implement `context.Context` but neither is the exact `context.Context` type, only the first will be used. This matches reasonable behavior (first argument convention), but should be documented.

**Template readability:** The nested `{{- if ... -}}{{- else if ... -}}{{- else -}}` structure with mixed indentation is hard to follow. This is an inherent limitation of Go template syntax in YAML, but adding inline comments (where the template format allows) or restructuring the nesting would help future maintainers.

---

## Test Coverage

### New unit tests: `TestSpanHierarchy` in both `contrib/graphql-go/graphql/graphql_test.go` and `contrib/99designs/gqlgen/tracer_test.go`

**Assessment:** Well-written tests that verify the exact parent-child span relationships using mocktracer. The assertions on `ParentID()` are the right way to test this. The comments explaining the expected chain (e.g., "parse, validate, and execute are chained because StartSpanFromContext context is propagated back through the graphql-go extension interface") are helpful.

**Minor concern:** `TestSpanHierarchy` in `graphql_test.go` expects exactly 5 spans (`require.Len(t, spans, 5)`). This is fragile if the graphql-go integration adds more spans in the future (e.g., for subscriptions or additional middleware). Consider using `require.GreaterOrEqual` or indexing by operation name rather than total count — though the current approach is acceptable since the test already indexes by operation name.

### Integration test updates: `internal/orchestrion/_integration/`

**99designs/gqlgen:** The `TopLevel.nested` span is correctly moved from being a direct child of the root to a child of `Query.topLevel`. This matches the fix to Bug 2 (GLS override) — previously the nested resolver incorrectly used the GLS-stored span (root) as parent instead of the topLevel resolver's span from the context chain.

**graphql-go:** The span hierarchy change is more significant. Previously `parse`, `validate`, `execute`, and `resolve` were all direct children of `graphql.server`. After the fix, they form a chain: `server -> parse -> validate -> execute -> resolve`. This is the correct behavior since `StartSpanFromContext` propagates the new span through the context chain, and subsequent phases start spans from that updated context.

**Concern — behavior change in graphql-go integration:** A reviewer (rarguelloF) explicitly flagged uncertainty about the graphql-go hierarchy change. The fix to Bug 2 (GLS priority reversal) causes the graphql-go spans to chain, whereas before they were all siblings of the root. Both `TestSpanHierarchy` and the updated integration test assert the chained behavior, which means the new behavior is intentional and tested. However, this is a **breaking change in span hierarchy for existing graphql-go users** — their dashboards, monitors, or alerts that assume `graphql.parse`, `graphql.validate`, and `graphql.execute` are all direct children of `graphql.server` will break. This should be called out prominently in the PR or release notes.

### New integration tests: `internal/orchestrion/_integration/dd-span/`

**Assessment:** The nil-guard tests (`spanWithNilNamedCtx`, `spanWithNilOtherCtx`) are well-targeted and verify the specific crash path (typed-nil interface causing a panic). The comment explaining why these appear as children of `test.root` (due to GLS fallback since `context.TODO()` has no span) is accurate and helpful.

---

## Generated Code Changes

The bulk of the diff (800+ lines) is in `contrib/99designs/gqlgen/internal/testserver/graph/generated.go`. This file is auto-generated by `github.com/99designs/gqlgen` and the changes reflect:

1. Upgrade from gqlgen v0.17.72 to v0.17.83 (the new `graphql.ResolveField` helper API)
2. Addition of the `TopLevel` resolver type needed for the new nested-span test

**Note:** The license header was removed from `generated.go` in this PR. This is because `generated.go` now starts with the standard `// Code generated by github.com/99designs/gqlgen, DO NOT EDIT.` comment, which is correct — the Datadog license header should not appear in files generated by third-party tools.

---

## Dependency Updates

`internal/orchestrion/_integration/go.mod` bumps multiple dependencies:
- `github.com/DataDog/orchestrion` from `v1.6.1` to `v1.8.1-0.20260312121543-8093b0b4eec9` (a pre-release SHA-pinned version)
- Various DataDog agent packages from v0.75.2 to v0.76.2
- gqlgen from v0.17.72 to v0.17.83

**Concern — pre-release Orchestrion dependency:** The `github.com/DataDog/orchestrion` dependency is pinned to a pre-release SHA (`v1.8.1-0.20260312121543-8093b0b4eec9`). This is noted in the PR as intentional — the PR is blocked on the corresponding Orchestrion PR being merged. The comment from darccio confirms this. Since the PR has now been merged, the Orchestrion dependency should have been updated to a stable release tag before merge. It's worth verifying post-merge whether this pre-release dependency was updated.

---

## Summary of Issues Found

| Severity | Location | Issue |
|----------|----------|-------|
| Medium | `ddtrace/tracer/context.go` | The claimed "Bug 1" fix (context.Background sentinel) does not appear in the diff — only the nil *Span guard is present. The GLS fallback for context.Background() may still be active. |
| Medium | `internal/orchestrion/_integration/graphql-go/graphql.go` | The span hierarchy change (siblings -> chain) is a breaking behavioral change for existing graphql-go users. Should be documented in release notes. |
| Low | `ddtrace/tracer/orchestrion.yml` | `__dd_ctxImpl` temp variable name could conflict with user code; consider a more unique name. |
| Low | `internal/orchestrion/context.go` | Zero unit test coverage on the new GLS priority-reversal lines per Codecov. Integration tests cover it indirectly. |
| Info | `internal/orchestrion/_integration/go.mod` | Orchestrion pinned to pre-release SHA; should be updated to stable tag if available. |

---

## Overall Assessment

The PR correctly identifies and fixes real bugs in the GLS/context span propagation system, with well-motivated explanations and good test coverage for the most important cases. The core changes (GLS lookup order reversal and the `//dd:span` template extension for interface-implementing contexts) are correct and well-implemented. The main concerns are the potential incompleteness of Bug 1's fix, the undocumented breaking change to graphql-go span hierarchy, and the pre-release Orchestrion dependency. The PR was approved by four reviewers and had thorough review iterations, particularly around the graphql-go hierarchy change.
