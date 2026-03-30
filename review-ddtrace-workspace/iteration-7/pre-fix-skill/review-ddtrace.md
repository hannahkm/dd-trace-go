# /review-ddtrace — Code review for dd-trace-go

Review code changes against the patterns and conventions that dd-trace-go reviewers consistently enforce. This captures the implicit standards that live in reviewers' heads but aren't in CONTRIBUTING.md.

Run this on a diff, a set of changed files, or a PR.

## How to use

If `$ARGUMENTS` contains a PR number or URL, fetch and review that PR's diff.
If `$ARGUMENTS` contains file paths, review those files.
If `$ARGUMENTS` is empty, review the current unstaged and staged git diff.

## Review approach

1. Read the diff to understand what changed and why.
2. Determine which reference files to consult based on what's in the diff:
   - **Always read** `.claude/review-ddtrace/style-and-idioms.md` — these patterns apply to all Go code in this repo.
   - **Read if the diff touches concurrency** (mutexes, atomics, goroutines, channels, sync primitives, or shared state): `.claude/review-ddtrace/concurrency.md`
   - **Read if the diff touches `contrib/`**: `.claude/review-ddtrace/contrib-patterns.md`
   - **Read if the diff touches hot paths** (span creation, serialization, sampling, payload encoding, tag setting) or adds/changes benchmarks: `.claude/review-ddtrace/performance.md`
3. Review the diff against the loaded guidance. Focus on issues the guidance specifically calls out — these come from real review feedback that was given repeatedly over the past 3 months.
4. Report findings using the output format below.

## Universal checklist

These are the highest-frequency review comments across the repo. Check every diff against these:

### Happy path left-aligned
The single most repeated review comment. Guard clauses and error returns should come first so the main logic stays at the left edge. If you see an `if err != nil` or an edge-case check that wraps the happy path in an else block, flag it.

```go
// Bad: happy path nested
if condition {
    // lots of main logic
} else {
    return err
}

// Good: early return, happy path left-aligned
if !condition {
    return err
}
// main logic here
```

### Regression tests for bug fixes
If the PR fixes a bug, there should be a test that reproduces the original bug. Reviewers ask for this almost every time it's missing.

### Don't silently drop errors
If a function returns an error, handle it. Logging at an appropriate level counts as handling. Silently discarding errors (especially from marshaling, network calls, or state mutations) is a recurring source of review comments.

### Named constants over magic strings/numbers
Use constants from `ddtrace/ext`, `instrumentation`, or define new ones. Don't scatter raw string literals like `"_dd.svc_src"` or protocol names through the code. If the constant already exists somewhere in the repo, import and use it.

### Don't add unused API surface
If a function, type, or method is not yet called anywhere, don't add it. Reviewers consistently push back on speculative API additions.

### Don't export internal-only functions
Functions meant for internal use should not follow the `WithX` naming pattern or be exported. `WithX` is the public configuration option convention — don't use it for internal plumbing.

### Extract shared/duplicated logic
If you see the same 3+ lines repeated across call sites, extract a helper. But don't create premature abstractions for one-time operations.

### Config through proper channels
- Environment variables must go through `internal/env` (or `instrumentation/env` for contrib), never raw `os.Getenv`. Note: `internal.BoolEnv` and similar helpers in the top-level `internal` package are **not** the same as `internal/env` — they are raw `os.Getenv` wrappers that bypass the validated config pipeline. Code should use `internal/env.Get`/`internal/env.Lookup` or the config provider, not `internal.BoolEnv`.
- Config loading belongs in `internal/config/config.go`'s `loadConfig`, not scattered through `ddtrace/tracer/option.go`.
- See CONTRIBUTING.md for the full env var workflow.

### Nil safety and type assertion guards
Multiple P1 bugs in this repo come from nil-typed interface values and unguarded type assertions. When casting a concrete type to an interface (like `context.Context`), a nil pointer of the concrete type produces a non-nil interface that panics on method calls. Guard with a nil check before the cast. Similarly, prefer type switches or comma-ok assertions over bare type assertions in code paths that handle user-provided or externally-sourced values.

### Error messages should describe impact
When logging a failure, explain what the user loses — not just what failed. Reviewers flag vague messages like `"failed to create admin client: %s"` and ask for impact context like `"failed to create admin client for cluster ID; cluster.id will be missing from DSM spans: %s"`. This helps operators triage without reading source code.

### Encapsulate internal state behind methods
When a struct has internal fields that could change representation (like a map being replaced with a typed struct), consumers should access data through methods, not by reaching into fields directly. Reviewers flag `span.meta[key]` style access and ask for `span.meta.Get(key)` — this decouples callers from the internal layout and makes migrations easier.

### Don't check in local/debug artifacts
Watch for `.claude/settings.local.json`, debugging `fmt.Println` leftovers, or commented-out test code. These get flagged immediately.

## Output format

Group findings by severity. Use inline code references (`file:line`).

**Blocking** — Must fix before merge (correctness bugs, data races, silent error drops, API surface problems).

**Should fix** — Strong conventions that reviewers will flag (happy path alignment, missing regression tests, magic strings, naming).

**Nits** — Style preferences that improve readability but aren't blocking (import grouping, comment wording, minor naming).

For each finding, briefly explain *why* (what could go wrong, or what convention it violates) rather than just stating the rule. Keep findings concise — one or two sentences each.

If the code looks good against all loaded guidance, say so. Don't manufacture issues.
