# Style and Idioms Reference

Patterns that dd-trace-go reviewers consistently enforce across all packages. These come from 3 months of real review feedback.

## Happy path left-aligned (highest frequency)

This is the most common single piece of review feedback. The principle: error/edge-case handling should return early, keeping the main logic at the left margin.

```go
// Reviewers flag this pattern:
if cond {
    doMainWork()
} else {
    return err
}

// Preferred:
if !cond {
    return err
}
doMainWork()
```

Real examples from reviews:
- Negating a condition to return early instead of wrapping 10+ lines in an if block
- Converting `if dsm && brokerAddr` nesting into `if !dsm || len(brokerAddrs) == 0 { return }`
- Flattening nested error handling in URL parsing

A specific variant: "not a blocker, but a specific behavior for a specific key is not what I'd call the happy path." Key-specific branches (like `if key == keyDecisionMaker`) should be in normal `if` blocks, not positioned as the happy path.

## Naming conventions

### Go initialisms
Use standard Go capitalization for initialisms: `OTel` not `Otel`, `ID` not `Id`. This applies to struct fields, function names, and comments.

```go
logsOTelEnabled  // not logsOtelEnabled
LogsOTelEnabled() // not LogsOtelEnabled()
```

### Function/method naming
- Use Go style for unexported helpers: `processTelemetry` not `process_Telemetry`
- Test functions: `TestResolveDogstatsdAddr` not `Test_resolveDogstatsdAddr`
- Prefer descriptive names over generic ones: `getRateLocked` tells you more than `getRate2`
- If a function returns a single value, the name should hint at the return: `defaultServiceName` not `getServiceConfig`

### Naming things clearly
Reviewers push back when names don't convey intent:
- "Shared" is unclear — `ReadOnly` better expresses the impact (`IsReadOnly`, `MarkReadOnly`)
- Don't name things after implementation details — name them after what they mean to callers
- If a field's role isn't obvious from context, the name should compensate (e.g., `sharedAttrs` or `promotedAttrs` instead of just `attrs`)

## Constants and magic values

Use named constants instead of inline literals:

```go
// Reviewers flag:
if u.Scheme == "unix" || u.Scheme == "http" || u.Scheme == "https" { ... }

// Preferred: define or reuse constants
const (
    schemeUnix  = "unix"
    schemeHTTP  = "http"
    schemeHTTPS = "https"
)
```

Specific patterns:
- String tag keys: import from `ddtrace/ext` or `instrumentation` rather than hardcoding `"_dd.svc_src"`
- Protocol identifiers, retry intervals, and timeout values should be named constants with comments explaining the choice
- If a constant already exists in `ext`, `instrumentation`, or elsewhere in the repo, use it rather than defining a new one

### Bit flags and magic numbers
Name bitmap values and numeric constants. "Let's name these magic bitmap numbers" is a direct quote from a review.

## Avoid unnecessary aliases and indirection

Reviewers push back on type aliases and function aliases that don't add value:

```go
// Flagged: "you love to create these aliases and I hate them"
type myAlias = somePackage.Type

// Also flagged: wrapping a function just to rename it
func doThing() { somePackage.DoThing() }
```

Only create aliases when there's a genuine need (avoiding import cycles, providing a cleaner public API).

## Import grouping

Follow the standard Go convention with groups separated by blank lines:
1. Standard library
2. Third-party packages
3. Datadog packages (`github.com/DataDog/...`)

Reviewers consistently suggest corrections when imports aren't grouped this way.

## Use standard library when available

Prefer standard library or `golang.org/x` functions over hand-rolled equivalents:
- `slices.Contains` instead of a custom `contains` helper
- `slices.SortStableFunc` instead of implementing `sort.Interface`
- `cmp.Or` for defaulting values
- `for range b.N` instead of `for i := 0; i < b.N; i++` (Go 1.22+)

## Comments and documentation

### Godoc accuracy
Comments that appear in godoc should be precise. Reviewers flag comments that are slightly wrong or misleading, like `// IsSet returns true if the key is set` when the actual behavior checks for non-empty values.

### Don't pin comments to specific files
```go
// Bad: "A zero value uses the default from option.go"
// Good: "A zero value uses defaultAgentInfoPollInterval."
```
Files move. Reference the constant or concept, not the file location.

### Explain "why" for non-obvious config
For feature flags, polling intervals, and other tunables, add a brief comment explaining the rationale, not just what the field does:
```go
// agentInfoPollInterval controls how often we refresh /info.
// A zero value uses defaultAgentInfoPollInterval.
agentInfoPollInterval time.Duration
```

### Comments for hooks and callbacks
When implementing interface methods that serve as hooks (like franz-go's `OnProduceBatchWritten`, `OnFetchBatchRead`), add a comment explaining when the hook is called and what it does — these aren't obvious to someone reading the code later.

## Code organization

### Function length
If a function is getting long (reviewers flag this as "too many lines in an already long function"), extract focused helper functions. Good candidates:
- Building a struct with complex initialization logic
- Parsing/validation sequences
- Repeated conditional blocks

### File organization
- Put types/functions in the file where they logically belong. Don't create a `record.go` for functions that should be in `tracing.go`.
- If a file grows too large, split along domain boundaries, not arbitrarily.
- Test helpers that mutate global state should be in `_test.go` files or build-tagged files, not shipped in production code.

### Don't combine unrelated getters
If two values are always fetched independently, don't bundle them into one function. `getSpanID()` and `getResource()` are better as separate methods than a combined `getSpanIDAndResource()`.

## Avoid unnecessary aliases and indirection

Reviewers push back on type aliases and function wrappers that don't add value:

```go
// Flagged: "you love to create these aliases and I hate them"
type myAlias = somePackage.Type

// Also flagged: wrapping a function just to rename it
func doThing() { somePackage.DoThing() }
```

Only create aliases when there's a genuine need (avoiding import cycles, providing a cleaner public API). If a one-liner wrapper exists solely to adapt a type at a single call site, consider inlining the call instead.

## Avoid `init()` functions

`init()` is unpopular in Go code in this repo. Reviewers ask to replace it with named helper functions called from variable initialization:

```go
// Flagged: "init() is very unpopular for go"
func init() {
    cfg.rootSessionID = computeSessionID()
}

// Preferred: explicit helper
var cfg = &config{
    rootSessionID: computeRootSessionID(),
}
```

The exception is `instrumentation.Load()` calls in contrib packages, which are expected to use `init()` per the contrib README.

## Embed interfaces for forward compatibility

When wrapping a type that implements an interface, embed the interface rather than proxying every method individually. This way, new methods added to the interface in future versions are automatically forwarded:

```go
// Fragile: must manually add every new method
type telemetryExporter struct {
    inner metric.Exporter
}
func (t *telemetryExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
    return t.inner.Export(ctx, rm)
}

// Better: embed so new methods are forwarded automatically
type telemetryExporter struct {
    metric.Exporter  // embed the interface
}
```

## Deprecation markers
When marking functions as deprecated, use the Go-standard `// Deprecated:` comment prefix so that linters and IDEs flag usage:
```go
// Deprecated: Use [Wrap] instead.
func Middleware(service string, opts ...Option) echo.MiddlewareFunc {
```

## Generated files
Maintain ordering in generated files. If a generated file like `supported_configurations.gen.go` has sorted keys, don't hand-edit in a way that breaks the sort — it'll cause confusion when the file is regenerated.
