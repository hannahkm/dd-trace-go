# Performance Reference

dd-trace-go runs in every instrumented Go service. Performance regressions directly impact customer applications. Reviewers are vigilant about hot-path changes.

## Benchmark before and after

When changing code in hot paths (span creation, tag setting, serialization, sampling), reviewers expect benchmark comparisons:

> "I'd recommend benchmarking the old implementation against the new."
> "This should be benchmarked and compared with `Tag(ext.ServiceName, ...)`. I think it's going to introduce an allocation in a really hot code path."

Run `go test -bench` before and after, and include the comparison in your PR description.

## Inlining cost awareness

On hot-path functions in `ddtrace/tracer/`, reviewers sometimes verify inlining with `go build -gcflags="-m=2"`. If a change grows a function past the compiler's inlining budget, wrap cold-path code (like error logging) in a `//go:noinline` helper to keep the hot caller inlineable.

## Avoid allocations in hot paths

### Pre-compute sizes
When building slices for serialization, compute the size upfront to avoid intermediate allocations:

```go
// Reviewed: "This causes part of the execution time regressions"
// The original code allocated a map then counted its length
// Better: count directly
size := len(span.metrics) + len(span.metaStruct)
for k := range span.meta {
    if k != "_dd.span_links" {
        size++
    }
}
```

### Avoid unnecessary byte slice allocation
When appending to a byte buffer, don't allocate intermediate slices:

```go
// Bad: allocates a temporary slice
tmp := make([]byte, 0, idLen+9)
tmp = append(tmp, checkpointID)
// ...
dst = append(dst, tmp...)

// Good: append directly to destination
dst = append(dst, checkpointID)
dst = binary.BigEndian.AppendUint64(dst, uint64(timestamp))
dst = append(dst, byte(idLen))
dst = append(dst, transactionID[:idLen]...)
```

### String building
Per CONTRIBUTING.md: favor `strings.Builder` or string concatenation (`a + "b" + c`) over `fmt.Sprintf` in hot paths.

## Lock contention in hot paths

### Don't call TracerConf() per span
`TracerConf()` acquires a lock and copies config data. Calling it on every span creation (e.g., inside `setPeerService`) creates lock contention and unnecessary allocations:

> "We are acquiring the lock and iterating over and copying internalconfig's PeerServiceMappings map on every single span, just to ultimately query the map by a key value."

Cache what you need at a higher level, or restructure to avoid per-span config reads.

### Minimize critical section scope
Get in and out of critical sections quickly. Don't do I/O, allocations, or complex logic while holding a lock.

## Serialization correctness

### Array header counts must match actual entries
When encoding msgpack arrays, the declared count must match the number of entries actually written. If entries can be skipped (e.g., a `meta_struct` value fails to serialize), the count will be wrong and downstream decoders will corrupt:

> "meta_struct entries are conditionally skipped when `msgp.AppendIntf` fails in the loop below; this leaves the encoded array shorter than the declared length"

Either pre-validate entries, use a two-pass approach (serialize then count), or adjust the header retroactively.

## Profiler-specific concerns

### Measure overhead for new profile types
New profile types (like goroutine leak detection) can impact application performance through STW pauses. Reviewers expect overhead analysis:

> "Did you look into the overhead for this profile type?"

Reference relevant research (papers, benchmarks) when introducing profile types that interact with GC or runtime internals.

### Concurrent profile capture ordering
Be aware of how profile types interact when captured concurrently. For example, a goroutine leak profile that waits for a GC cycle will cause the heap profile to reflect the *previous* cycle's data, not the current one.

