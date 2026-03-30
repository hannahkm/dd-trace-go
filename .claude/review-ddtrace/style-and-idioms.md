# Style and Idioms Reference

dd-trace-go-specific patterns reviewers consistently enforce. General Go conventions (naming, formatting, error handling) are covered by [Effective Go](https://go.dev/doc/effective_go) — this file focuses on what's specific to this repo.

## Happy path left-aligned (highest frequency)

Error/edge-case handling should return early, keeping the main logic at the left margin.

```go
// Bad:
if cond {
    doMainWork()
} else {
    return err
}

// Good:
if !cond {
    return err
}
doMainWork()
```

## Naming conventions

### Go initialisms
Use standard Go capitalization for initialisms: `OTel` not `Otel`, `ID` not `Id`. This applies to struct fields, function names, and comments.

```go
logsOTelEnabled  // not logsOtelEnabled
LogsOTelEnabled() // not LogsOtelEnabled()
```

### Function/method naming
- Prefer `getRateLocked` over `getRate2` — the suffix should convey intent (in this case, that the lock must be held)
- Functions that expect to be called with a lock already held should be named `*Locked` (e.g., `getRateLocked`) so the contract is visible at call sites

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
Name bitmap values and numeric constants.

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
When implementing interface methods that serve as hooks or callbacks, add a comment explaining when the hook is called and what it does — these aren't obvious to someone reading the code later.

## Avoid unnecessary aliases and indirection

Don't create type aliases or function wrappers that don't add value:

```go
// Bad:
type myAlias = somePackage.Type
func doThing() { somePackage.DoThing() }

// Only alias when genuinely needed (import cycles, cleaner public API)
```

## Avoid `init()` functions

Avoid `init()` in this repo. Use named helper functions called from variable initialization instead:

```go
// Bad:
func init() {
    cfg.rootSessionID = computeSessionID()
}

// Good:
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
