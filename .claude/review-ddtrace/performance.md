# Performance Reference

dd-trace-go runs in every instrumented Go service. Performance regressions directly impact customer applications. Reviewers are vigilant about hot-path changes.

## Benchmark before and after

When changing code in hot paths (span creation, tag setting, serialization, sampling), include before/after benchmark comparisons in the PR description. Run `go test -bench` on both the old and new code.

## Inlining cost awareness

On hot-path functions in `ddtrace/tracer/`, reviewers sometimes verify inlining with `go build -gcflags="-m=2"`. If a change grows a function past the compiler's inlining budget, wrap cold-path code (like error logging) in a `//go:noinline` helper to keep the hot caller inlineable.

## Avoid allocations in hot paths

### Pre-compute sizes
When building slices for serialization, compute the size upfront rather than growing dynamically.

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

### Cache config reads outside hot loops
Don't call lock-acquiring config accessors (like `TracerConf()`) on every span. Cache what you need at a higher level to avoid per-span lock contention and allocations.

### Minimize critical section scope
Get in and out of critical sections quickly. Don't do I/O, allocations, or complex logic while holding a lock.

## Serialization correctness

### Array header counts must match actual entries
When encoding msgpack arrays, the declared count must match the number of entries actually written. If entries can be conditionally skipped (e.g., a value fails to serialize), the count will be wrong and downstream decoders will corrupt. Either pre-validate entries, use a two-pass approach (serialize then count), or adjust the header retroactively.

## Profiler-specific concerns

### Measure overhead for new profile types
New profile types can impact application performance through STW pauses or GC triggers. Include overhead analysis and reference relevant benchmarks when introducing profile types that interact with GC or runtime internals.

### Concurrent profile capture ordering
Be aware of how profile types interact when captured concurrently. For example, a goroutine leak profile that waits for a GC cycle will cause the heap profile to reflect the *previous* cycle's data, not the current one.

