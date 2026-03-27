# Contrib Integration Patterns Reference

Patterns specific to `contrib/` packages. These come from review feedback on integration PRs (kafka, echo, gin, AWS, SQL, MCP, franz-go, etc.).

## API design for integrations

### Don't return custom wrapper types
Prefer hooks/options over custom client types. Reviewers pushed back strongly on a `*Client` wrapper:

> "This library natively supports tracing with the `WithHooks` option, so I don't think we need to return this custom `*Client` type (returning custom types is something we tend to avoid as it makes things more complicated, especially with Orchestrion)."

When the instrumented library supports hooks or middleware, use those. Return `kgo.Opt` or similar library-native types, not a custom struct wrapping the client.

### WithX is for user-facing options only
The `WithX` naming convention is reserved for public configuration options that users pass when initializing an integration. Don't use `WithX` for internal plumbing:

```go
// Bad: internal-only function using public naming convention
func WithClusterID(id string) Option { ... }

// Good: unexported setter for internal use
func (tr *Tracer) setClusterID(id string) { ... }
```

If a function won't be called by users, don't export it.

### Service name conventions
Service names in integrations follow a specific pattern:

- Most integrations use optional `WithService(name)` — the service name is NOT a mandatory argument
- Some legacy integrations (like gin's `Middleware(serviceName, ...)`) have mandatory service name parameters. These are considered legacy and shouldn't be replicated in new integrations.
- The default service name should be derived from the package's `componentName` (via `instrumentation.PackageXxx`), not a new string
- Track where the service name came from using `_dd.svc_src` (service source). Import the tag key from `ext` or `instrumentation`, don't hardcode it
- Service source values should come from established constants, not ad-hoc strings

### Span options must be request-local
Never append to a shared slice of span options from concurrent request handlers:

```go
// Bug: races when concurrent HTTP requests append to shared slice
options := append(r.config.spanOpts, tracer.ServiceName(svc))
```

Copy the options slice before appending per-request values. This was flagged as P1 in multiple contrib PRs.

## Async work and lifecycle

### Async work must be cancellable on Close
When an integration starts background goroutines (e.g., fetching Kafka cluster IDs), they must be cancellable when the user calls `Close()`:

> "One caveat of doing this async - we use the underlying producer/consumer so need this to finish before closing."

Use a context with cancellation:

```go
type wrapped struct {
    closeAsync []func() // functions to call on Close
}

func (w *wrapped) Close() error {
    for _, fn := range w.closeAsync {
        fn() // cancels async work
    }
    return w.inner.Close()
}
```

### Don't block user code for observability
Users don't expect their observability library to add latency to their application. When reviewing any synchronous wait in an integration's startup or request path, actively question whether the timeout is acceptable. Reviewers flag synchronous waits:

> "How critical *is* cluster ID? Enough to block for 2s? Even 2s could be a nuisance to users' environments; I don't believe they expect their observability library to block their services."

### Suppress expected cancellation noise
When `Close()` cancels a background lookup, the cancellation is expected — don't log it as a warning:

```go
// Bad: noisy warning on expected cancellation
if err != nil {
    log.Warn("failed to fetch cluster ID: %s", err)
}

// Good: only warn on unexpected errors
if err != nil && !errors.Is(err, context.Canceled) {
    log.Warn("failed to fetch cluster ID: %s", err)
}
```

### Error messages should describe impact
When logging failures, explain what is lost:

```go
// Vague:
log.Warn("failed to create admin client: %s", err)

// Better: explains impact
log.Warn("failed to create admin client for cluster ID; cluster.id will be missing from DSM spans: %s", err)
```

## Data Streams Monitoring (DSM) patterns

### Check DSM processor availability before tagging spans
Don't tag spans with DSM metadata when DSM is disabled — it wastes cardinality:

```go
// Bad: tags spans even when DSM is off
tagActiveSpan(ctx, transactionID, checkpointName)
if p := datastreams.GetProcessor(ctx); p != nil {
    p.TrackTransaction(...)
}

// Good: check first
if p := datastreams.GetProcessor(ctx); p != nil {
    tagActiveSpan(ctx, transactionID, checkpointName)
    p.TrackTransaction(...)
}
```

### Function parameter ordering
For DSM functions dealing with cluster/topic/partition, order hierarchically: cluster > topic > partition. Reviewers flag reversed ordering.

### Deduplicate with timestamp variants
When you have both `DoThing()` and `DoThingAt(timestamp)`, have the first call the second:

```go
func TrackTransaction(ctx context.Context, id, name string) {
    TrackTransactionAt(ctx, id, name, time.Now())
}
```

## Integration testing

### Consistent patterns across similar integrations
When implementing a feature (like DSM cluster ID fetching) that already exists in another integration (e.g., confluent-kafka), follow the existing pattern. Reviewers flag inconsistencies between similar integrations, like using `map + mutex` in one and `sync.Map` in another.

### Orchestrion compatibility
Be aware of Orchestrion (automatic instrumentation) implications:
- The `orchestrion.yml` in contrib packages defines instrumentation weaving
- Be careful with context parameters — `ArgumentThatImplements "context.Context"` can produce invalid code when the parameter is already named `ctx`
- Guard against nil typed interface values: a `*CustomContext(nil)` cast to `context.Context` produces a non-nil interface that panics on `Value()`

## Consistency across similar integrations

When a feature exists in one integration (e.g., cluster ID fetching in confluent-kafka), implementations in similar integrations (e.g., Shopify/sarama, IBM/sarama, segmentio/kafka-go) should follow the same patterns. Reviewers flag inconsistencies like:
- Using `map + sync.Mutex` in one package and `sync.Map` in another for the same purpose
- Different error handling strategies for the same failure mode
- One integration trimming whitespace from bootstrap servers while another doesn't

When reviewing a contrib PR, check whether the same feature exists in a related integration and whether the approach is consistent.

## Span tags and metadata

### Required tags for integration spans
Per the contrib README:
- `span.kind`: set in root spans (`client`, `server`, `producer`, `consumer`). Omit if `internal`.
- `component`: set in all spans, value is the integration's full package path

### Resource name changes
Changing the resource name format is a potential breaking change for the backend. Ask: "Is this a breaking change for the backend? Or is it handled by it so resource name is virtually the same as before?"
