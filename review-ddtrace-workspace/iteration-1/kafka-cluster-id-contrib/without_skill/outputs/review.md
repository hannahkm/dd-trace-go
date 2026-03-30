# Code Review: PR #4470 -- feat(dsm): add kafka_cluster_id to confluent-kafka-go

## Summary

This PR adds `kafka_cluster_id` enrichment to the confluent-kafka-go DSM (Data Streams Monitoring) integration. It launches an async goroutine on consumer/producer creation to query the Kafka admin API for the cluster ID, then includes this ID in DSM edge tags, span tags, and backlog offset tracking. The implementation is non-blocking and cancellable on Close().

---

## Blocking

### 1. TOCTOU race on `ClusterID()` reads -- double read can yield inconsistent values

**Files:**
- `contrib/confluentinc/confluent-kafka-go/kafkatrace/dsm.go:53-54`
- `contrib/confluentinc/confluent-kafka-go/kafkatrace/dsm.go:73-74`
- `contrib/confluentinc/confluent-kafka-go/kafkatrace/consumer.go:70-71`
- `contrib/confluentinc/confluent-kafka-go/kafkatrace/producer.go:62-63` (lines from the diff context for `StartProduceSpan`)

In multiple places the code calls `tr.ClusterID()` twice in succession -- once for the guard check and once for the value:

```go
if tr.ClusterID() != "" {
    edges = append(edges, "kafka_cluster_id:"+tr.ClusterID())
}
```

Because `SetClusterID` is called from a concurrent goroutine, the value could change between the two calls. In the common case this means the first call returns `""` and the second returns the real ID (or vice versa). While the RWMutex ensures no torn reads, the inconsistency means:
- The check passes but the appended value is different from what was checked.
- Or the check fails (returns `""`) but by the time the tag would be used, the ID is available.

**Fix:** Read the cluster ID once into a local variable:
```go
if cid := tr.ClusterID(); cid != "" {
    edges = append(edges, "kafka_cluster_id:"+cid)
}
```

This is a minor data race in terms of practical impact (worst case: one message misses or gets a stale cluster ID), but it is a correctness pattern issue that should be fixed given this is a library consumed widely.

---

## Should Fix

### 2. Code duplication between `kafka/` and `kafka.v2/` packages

**Files:**
- `contrib/confluentinc/confluent-kafka-go/kafka/kafka.go`
- `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go`

The `startClusterIDFetch` function is copy-pasted identically between the two packages (the v1 and v2 confluent-kafka-go wrappers). The only difference is the import path for `kafka.AdminClient`. This is an existing pattern in the codebase (the two packages have always been near-duplicates), but it is worth noting for maintainability. If feasible, consider extracting the non-kafka-type-dependent logic into the shared `kafkatrace` package, since the `Tracer` type already lives there. The admin client creation would remain in each package, but the goroutine/cancellation logic could be shared.

### 3. Context variable shadowing obscures cancellation semantics

**Files:**
- `contrib/confluentinc/confluent-kafka-go/kafka/kafka.go:60-65`
- `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:60-65`

Inside `startClusterIDFetch`, the inner `ctx, cancel` from `context.WithTimeout` shadows the outer `ctx, cancel` from `context.WithCancel`:

```go
ctx, cancel := context.WithCancel(context.Background())       // outer
done := make(chan struct{})
go func() {
    defer close(done)
    defer admin.Close()
    ctx, cancel := context.WithTimeout(ctx, 2*time.Second)    // shadows outer ctx, cancel
    defer cancel()
    clusterID, err := admin.ClusterID(ctx)
    if err != nil {
        if ctx.Err() == context.Canceled {                     // checks inner ctx
```

This works correctly because the inner context is a child of the outer one, so cancelling the outer propagates to the inner. However, the shadowing makes the code harder to reason about -- a reader must carefully trace which `ctx` and `cancel` are in scope. Consider renaming to make the relationship explicit:

```go
ctx, cancel := context.WithCancel(context.Background())
...
go func() {
    ...
    timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 2*time.Second)
    defer timeoutCancel()
    clusterID, err := admin.ClusterID(timeoutCtx)
    ...
}
```

### 4. The `ctx.Err()` check after `ClusterID` failure does not distinguish timeout from external cancellation

**Files:**
- `contrib/confluentinc/confluent-kafka-go/kafka/kafka.go:69-71`
- `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:69-71`

```go
if ctx.Err() == context.Canceled {
    return
}
instr.Logger().Warn("failed to fetch Kafka cluster ID: %s", err)
```

When the 2-second timeout fires, `ctx.Err()` returns `context.DeadlineExceeded`, not `context.Canceled`. The warning log will fire for timeouts (which is correct). However, if the outer cancel and the timeout fire at nearly the same time, the inner context's `Err()` could return either `Canceled` or `DeadlineExceeded` depending on ordering. This is fine in practice but the intent would be clearer by checking the **parent** context:

```go
if parentCtx.Err() == context.Canceled {
    // Close() was called, suppress the warning
    return
}
```

This disambiguates "we were told to stop" from "the API timed out."

### 5. Tests wait for cluster ID with `require.Eventually` but don't account for DSM-disabled code paths

**Files:**
- `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka_test.go:186`
- `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka_test.go:194`
- `contrib/confluentinc/confluent-kafka-go/kafka/kafka_test.go:401`
- `contrib/confluentinc/confluent-kafka-go/kafka/kafka_test.go:409`

The `produceThenConsume` helper unconditionally adds `require.Eventually` waits for the cluster ID:

```go
require.Eventually(t, func() bool { return p.tracer.ClusterID() != "" }, 5*time.Second, 10*time.Millisecond)
```

But `produceThenConsume` is called from multiple tests, some of which may not enable DSM (e.g., `WithDataStreams()` is not always passed). When DSM is not enabled, the cluster ID fetch goroutine is never started, so `ClusterID()` will always return `""`, and this `require.Eventually` will block for 5 seconds and then fail the test.

Looking at the test code more carefully: in the `kafka.v2/kafka_test.go` version, the `produceThenConsume` function has a `useProducerEventsChannel` boolean parameter, while the `kafka/kafka_test.go` version does not. The existing callers (e.g., `TestConsumerFunctional`) pass `WithDataStreams()` in the functional tests that exercise this path. However, if any future caller of `produceThenConsume` omits `WithDataStreams()`, the test will fail with a confusing 5-second timeout rather than a clear error message. Consider guarding the `require.Eventually` on whether DSM is enabled:

```go
if p.tracer.DSMEnabled() {
    require.Eventually(t, func() bool { return p.tracer.ClusterID() != "" }, 5*time.Second, 10*time.Millisecond)
}
```

---

## Nits

### 6. Warn log uses `%s` for error formatting; prefer `%v`

**Files:**
- `contrib/confluentinc/confluent-kafka-go/kafka/kafka.go:72`
- `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:72`
- `contrib/confluentinc/confluent-kafka-go/kafka/kafka.go:66` (in WrapConsumer)
- `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:66` (in WrapConsumer)
- (and similar in WrapProducer)

```go
instr.Logger().Warn("failed to fetch Kafka cluster ID: %s", err)
```

Go convention is to use `%v` for errors (or `%w` in `fmt.Errorf`). While `%s` works (it calls `Error()` under the hood), `%v` is the idiomatic choice.

### 7. The `TestClusterIDConcurrency` test writer always writes the same value

**File:** `contrib/confluentinc/confluent-kafka-go/kafkatrace/tracer_test.go:75-77`

```go
wg.Go(func() {
    for range numIterations {
        tr.SetClusterID(fmt.Sprintf("cluster-%d", 0))
    }
})
```

The writer always writes `"cluster-0"`. The loop variable is hardcoded to `0`, so `fmt.Sprintf("cluster-%d", 0)` always produces the same string. This doesn't exercise the race detector as thoroughly as it could. Consider using the iteration index:

```go
for i := range numIterations {
    tr.SetClusterID(fmt.Sprintf("cluster-%d", i))
}
```

### 8. Minor: `closeAsync` slice is never pre-allocated

**Files:**
- `contrib/confluentinc/confluent-kafka-go/kafka/kafka.go` (Consumer and Producer structs)
- `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go` (Consumer and Producer structs)

The `closeAsync` slice is appended to with `append(wrapped.closeAsync, ...)` without pre-allocation. Currently there is only ever one entry, so this is fine. If more async jobs are added in the future, consider initializing with `make([]func(), 0, 1)`. This is extremely minor and not worth changing unless more items are expected.

### 9. `TrackKafkaHighWatermarkOffset` docstring update is incomplete

**File:** `ddtrace/tracer/data_streams.go:77-78`

The comment for `TrackKafkaHighWatermarkOffset` says:
```go
// TrackKafkaHighWatermarkOffset should be used in the producer, to track when it produces a message.
```

But this function is used by the **consumer** to track high watermark offsets (as the code in `kafkatrace/dsm.go:25` `TrackHighWatermarkOffset` confirms -- it takes `offsets []TopicPartition, consumer Consumer`). The docstring was likely copied from `TrackKafkaProduceOffset` and not updated. This predates this PR but since the function signature was changed (the `_` placeholder for cluster was replaced with a real parameter), it would be a good time to fix the comment.

### 10. Consistent tag naming: `kafka_cluster_id` vs `messaging.kafka.cluster_id`

**Files:**
- `ddtrace/ext/messaging.go` (new constant `MessagingKafkaClusterID = "messaging.kafka.cluster_id"`)
- `contrib/confluentinc/confluent-kafka-go/kafkatrace/dsm.go` (edge tag uses `"kafka_cluster_id:"`)
- `internal/datastreams/processor.go` (backlog tag uses `"kafka_cluster_id:"`)

The span tag uses `messaging.kafka.cluster_id` (OpenTelemetry semantic convention style), while the DSM edge tags and backlog tags use `kafka_cluster_id`. This is likely intentional -- DSM tags have their own namespace separate from span tags -- but it is worth confirming that this naming split is consistent with the other language tracers (Java, Python, Node) referenced in the PR description.
