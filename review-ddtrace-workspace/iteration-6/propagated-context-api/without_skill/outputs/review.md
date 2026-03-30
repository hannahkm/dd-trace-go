# Code Review: PR #4492 — `tracer.StartSpanFromPropagatedContext`

**PR:** https://github.com/DataDog/dd-trace-go/pull/4492
**Author:** darccio (Dario Castañé)
**Summary:** Adds a new generic public API `StartSpanFromPropagatedContext[C TextMapReader]` that combines Extract + StartSpan into a single ergonomic call for starting spans from incoming distributed trace carriers.

---

## Overall Assessment

The PR is clean and well-motivated. The generic constraint approach (variant D from the RFC) is a good design choice: it enforces at compile time that callers pass a proper `TextMapReader` carrier instead of an opaque `any`, while still supporting HTTP headers, gRPC metadata, or any custom carrier without coupling `ddtrace/tracer` to `net/http`. The implementation is short and readable. Most issues below are minor, with one moderate concern around caller-visible behavioral differences vs. `StartSpanFromContext`.

---

## Issues

### 1. Missing `options.Expand` — potential data race if caller reuses opts slice

**Severity: Moderate**

`StartSpanFromContext` (the analogous function in `context.go`) explicitly copies the caller's slice before appending to it:

```go
// copy opts in case the caller reuses the slice in parallel
// we will add at least 1, at most 2 items
optsLocal := options.Expand(opts, 0, 2)
```

The new function appends directly to the `opts` parameter:

```go
func StartSpanFromPropagatedContext[C TextMapReader](ctx gocontext.Context, operationName string, carrier C, opts ...StartSpanOption) (*Span, gocontext.Context) {
    ...
    if spanCtx != nil {
        if links := spanCtx.SpanLinks(); len(links) > 0 {
            opts = append(opts, WithSpanLinks(links))
        }
        opts = append(opts, func(cfg *StartSpanConfig) { cfg.Parent = spanCtx })
    }
    opts = append(opts, withContext(ctx))
    span := tr.StartSpan(operationName, opts...)
```

In Go, a variadic `opts ...StartSpanOption` slice may or may not share its backing array with the caller's original slice depending on capacity. If the caller passes a slice with spare capacity and then reuses it concurrently (a real scenario in high-throughput servers that pre-allocate option slices), appending without copying can corrupt the caller's slice or cause a race. `options.Expand(opts, 0, 3)` (up to 3 items may be appended: WithSpanLinks, Parent, withContext) would protect against this the same way `StartSpanFromContext` does.

### 2. Span links from carrier are potentially duplicated when caller also passes `WithSpanLinks`

**Severity: Minor**

When a context is extracted that carries span links, the implementation prepends them into `opts` and then `spanStart` appends all opts' links into `span.spanLinks`. If the caller simultaneously passes `WithSpanLinks(someLinks)` via the `opts` parameter, both sets of links end up on the span — which is probably correct. However, the ordering is surprising: the carrier's links come first (prepended), then the caller's links. There is no deduplication.

More concretely, the test `span_links preservation` asserts `assert.Contains(t, span.spanLinks, link)` but does not assert that carrier links are also present, nor that there are no duplicates. This is not necessarily wrong, but the contract around link merging order should be documented in the godoc.

### 3. Missing `pprofCtxActive` propagation — inconsistency with `StartSpanFromContext`

**Severity: Minor (behavioral gap)**

`StartSpanFromContext` does this after calling `StartSpan`:

```go
s := StartSpan(operationName, optsLocal...)
if s != nil && s.pprofCtxActive != nil {
    ctx = s.pprofCtxActive
}
return s, ContextWithSpan(ctx, s)
```

The new function returns `ContextWithSpan(ctx, span)` without propagating `span.pprofCtxActive` into the returned context:

```go
span := tr.StartSpan(operationName, opts...)
return span, ContextWithSpan(ctx, span)
```

When profiler hotspots are enabled, `applyPPROFLabels` sets `span.pprofCtxActive` to a `pprof.WithLabels` context. If that context is not threaded back through the returned Go `context.Context`, any child spans started via the returned context will not inherit the correct pprof labels, degrading profiler accuracy. This is a latent bug if `StartSpanFromPropagatedContext` is used in code paths with profiler hotspots enabled.

The fix is:

```go
span := tr.StartSpan(operationName, opts...)
newCtx := ctx
if span != nil && span.pprofCtxActive != nil {
    newCtx = span.pprofCtxActive
}
return span, ContextWithSpan(newCtx, span)
```

### 4. `log.Debug` error message uses `.Error()` string — minor style inconsistency

**Severity: Nit**

```go
log.Debug("StartSpanFromPropagatedContext: failed to extract span context: %v", err.Error())
```

Elsewhere in tracer.go, `log.Debug` with `%v` is passed the error directly (not `.Error()`), since `%v` on an `error` already calls `.Error()`. For consistency:

```go
log.Debug("StartSpanFromPropagatedContext: failed to extract span context: %v", err)
```

### 5. `ErrSpanContextNotFound` is expected/normal — debug log fires on every untraced request

**Severity: Nit**

When no trace context is present in the carrier (the common case for fresh/untraced requests), `Extract` returns `ErrSpanContextNotFound`. The current code logs this at debug level:

```go
if err != nil && log.DebugEnabled() {
    log.Debug("StartSpanFromPropagatedContext: failed to extract span context: %v", err.Error())
}
```

This means every incoming untraced request will emit a debug log line. In contrast, the propagators internally already silently swallow `ErrSpanContextNotFound` (see `textmap.go` line 301: `if err != ErrSpanContextNotFound`). It would be more consistent with the rest of the codebase to suppress this expected error from the log, or at minimum to use `errors.Is(err, ErrSpanContextNotFound)` to distinguish missing context from actual malformed-carrier errors:

```go
if err != nil && !errors.Is(err, ErrSpanContextNotFound) && log.DebugEnabled() {
    log.Debug("StartSpanFromPropagatedContext: failed to extract span context: %v", err)
}
```

### 6. Setting `cfg.Parent` via inline closure instead of `ChildOf`

**Severity: Nit**

```go
opts = append(opts, func(cfg *StartSpanConfig) { cfg.Parent = spanCtx })
```

`ChildOf(spanCtx)` already does exactly this (and reads as self-documenting intent). However `ChildOf` is deprecated in favour of `Span.StartChild`. Since neither `ChildOf` nor `Span.StartChild` fits here (we have a `*SpanContext`, not a `*Span`), the inline closure is pragmatically correct. It is worth adding a brief comment to explain why the inline closure is used rather than the higher-level API, so future readers understand this is intentional and not an oversight.

---

## Test Coverage

The tests are comprehensive and readable: parent extraction, root span fallback, span links preservation, custom tag merging via opts, and HTTP headers carrier are all exercised. A few suggestions:

- **No race test**: `StartSpanFromContext` has `TestStartSpanFromContextRace` specifically testing concurrent use with a shared options slice. Given issue #1 above, a similar race test for `StartSpanFromPropagatedContext` would be valuable (and would fail before the `options.Expand` fix).
- **`ErrSpanContextNotFound` vs. other errors**: A test with a corrupted/malformed carrier would confirm the error logging behavior (issue #5).
- **`pprofCtxActive` propagation**: No test verifies that the returned context carries the correct pprof context when hotspots are enabled (issue #3).

---

## Documentation / Godoc

The godoc is good. One suggested addition: document the span links merge behavior explicitly — i.e., that links from the extracted carrier are prepended to any `WithSpanLinks` opts the caller passes, and that there is no deduplication.

---

## Summary Table

| # | Severity | Issue |
|---|----------|-------|
| 1 | Moderate | Missing `options.Expand` — potential data race on caller-reused opts slice |
| 2 | Minor    | Span link merge order undocumented; no dedup |
| 3 | Minor    | `pprofCtxActive` not propagated into returned context (inconsistency with `StartSpanFromContext`) |
| 4 | Nit      | `err.Error()` passed to `%v` format verb |
| 5 | Nit      | `ErrSpanContextNotFound` logged on every untraced request |
| 6 | Nit      | Inline closure instead of `ChildOf` — deserves a comment |

The most important change before merge is issue #1 (copy the opts slice) and issue #3 (pprofCtxActive propagation), both of which are bugs that cause the new function to behave differently from `StartSpanFromContext` in subtle ways.
