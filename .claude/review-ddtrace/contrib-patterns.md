# Contrib Integration Patterns Reference

Patterns specific to `contrib/` packages.

## API design for integrations

### Prefer library-native extension points over wrapper types
When the instrumented library supports hooks, middleware, or options, use those rather than returning a custom wrapper type. Custom wrappers complicate Orchestrion (automatic instrumentation) and force users to change their code.

### WithX is for user-facing options only
The `WithX` naming convention is reserved for public configuration options. Don't use it for internal plumbing:

```go
// Bad: internal-only function using public naming convention
func WithClusterID(id string) Option { ... }

// Good: unexported setter for internal use
func (tr *Tracer) setClusterID(id string) { ... }
```

### Service name conventions
- Most integrations use optional `WithService(name)` — the service name is NOT a mandatory argument
- Default service names should derive from the package's `componentName` (via `instrumentation.PackageXxx`)
- Track where the service name came from using `_dd.svc_src` (service source). Import the tag key from `ext` or `instrumentation`, don't hardcode it

### Span options must be request-local
Never append to a shared slice of span options from concurrent request handlers:

```go
// Bug: races when concurrent requests append to shared slice
options := append(r.config.spanOpts, tracer.ServiceName(svc))
```

Copy the options slice before appending per-request values.

## Async work and lifecycle

### Async work must be cancellable on Close
When an integration starts background goroutines, they must be cancellable when the user calls `Close()`. Use a context with cancellation and cancel it in `Close()`.

### Don't block user code for observability
Users don't expect their observability library to add latency. Question any synchronous wait in a startup or request path — if the data being fetched is optional (metadata, IDs), fetch it asynchronously.

### Suppress expected cancellation noise
When `Close()` cancels a background operation, don't log the cancellation as a warning:

```go
if err != nil && !errors.Is(err, context.Canceled) {
    log.Warn("failed to fetch metadata: %s", err)
}
```

### Error messages should describe impact
When logging failures, explain what the user loses — not just what failed:

```go
// Bad:
log.Warn("failed to create admin client: %s", err)

// Good:
log.Warn("failed to create admin client; cluster metadata will be missing from spans: %s", err)
```

## Data Streams Monitoring (DSM) patterns

These patterns apply anywhere DSM code appears — in `contrib/`, `ddtrace/tracer/`, or `datastreams/`.

### Gate DSM work on processor availability
Don't tag spans with DSM metadata or do DSM processing when DSM is disabled:

```go
// Bad: tags spans even when DSM is off
tagActiveSpan(ctx, id, name)
if p := datastreams.GetProcessor(ctx); p != nil {
    p.TrackTransaction(...)
}

// Good: check first
if p := datastreams.GetProcessor(ctx); p != nil {
    tagActiveSpan(ctx, id, name)
    p.TrackTransaction(...)
}
```

## Consistency across similar integrations

When a feature exists in one integration, implementations in related integrations should follow the same patterns. Check for:
- Same synchronization approach (don't use `map + sync.Mutex` in one package and `sync.Map` in another)
- Same error handling strategy for the same failure mode
- Same input normalization (e.g., trimming whitespace from addresses)

## Span tags and metadata

### Required tags for integration spans
Per the contrib README:
- `span.kind`: set in root spans (`client`, `server`, `producer`, `consumer`). Omit if `internal`.
- `component`: set in all spans, value is the integration's full package path

### Resource name changes
Changing the resource name format is a potential breaking change for the backend. Verify backward compatibility before changing resource name formatting.

### Orchestrion compatibility
Be aware of Orchestrion (automatic instrumentation) implications:
- The `orchestrion.yml` in contrib packages defines instrumentation weaving
- Guard against nil typed interface values: a nil pointer cast to an interface produces a non-nil interface that panics on method calls
