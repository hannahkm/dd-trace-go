# Code Review: PR #4470 - feat(dsm): add kafka_cluster_id to confluent-kafka-go

## Summary

This PR adds `kafka_cluster_id` tagging to the confluent-kafka-go contrib integration for Data Streams Monitoring (DSM). It launches an async goroutine during consumer/producer creation to fetch the cluster ID from the Kafka admin API, then enriches spans, DSM edge tags, and backlog metrics with that ID. The implementation mirrors patterns already established in the Shopify/sarama, IBM/sarama, and segmentio/kafka-go integrations.

---

## Blocking

### 1. TOCTOU race on `ClusterID()` in `SetConsumeCheckpoint` and `SetProduceCheckpoint`

**File:** `contrib/confluentinc/confluent-kafka-go/kafkatrace/dsm.go:53-54` (and `:73-74`)

```go
if tr.ClusterID() != "" {
    edges = append(edges, "kafka_cluster_id:"+tr.ClusterID())
}
```

`ClusterID()` is called twice without holding the lock across both calls. Since `SetClusterID` can be invoked concurrently from the background goroutine, there is a theoretical window where:
- First call returns `""` (not yet set), so the branch is skipped.
- Or first call returns a value, second call returns a *different* value (though unlikely for cluster ID, which is set once).

More practically, this is a TOCTOU pattern that should be fixed by reading the value once:

```go
if id := tr.ClusterID(); id != "" {
    edges = append(edges, "kafka_cluster_id:"+id)
}
```

The same pattern appears in `StartConsumeSpan` (`consumer.go:70-71`) and `StartProduceSpan` (`producer.go:65-66`). While the practical impact is low (cluster ID is written once and never changes), it is a correctness issue and every other read-site in the sarama/segmentio integrations captures the value in a local variable first.

---

## Should Fix

### 2. Inconsistent concurrency primitive: `sync.RWMutex` vs `atomic.Value`

**File:** `contrib/confluentinc/confluent-kafka-go/kafkatrace/tracer.go:31-32`

The Shopify/sarama (`contrib/Shopify/sarama/option.go:29`) and IBM/sarama (`contrib/IBM/sarama/option.go:27`) integrations both use `atomic.Value` with a `// +checkatomic` annotation for `clusterID`. The segmentio/kafka-go integration (`contrib/segmentio/kafka-go/internal/tracing/tracer.go:30`) does the same.

This PR introduces `sync.RWMutex` instead. While functionally correct, this is an unnecessary divergence from the established pattern used by all other Kafka integrations in this repo. `atomic.Value` is simpler, more performant for a write-once/read-many field, and consistent with the codebase convention. Using `sync.RWMutex` also means the `+checkatomic` static analysis annotation cannot be applied here.

**Recommendation:** Switch to `atomic.Value` to match the other Kafka integrations:

```go
clusterID atomic.Value // +checkatomic
```

```go
func (tr *Tracer) ClusterID() string {
    v, _ := tr.clusterID.Load().(string)
    return v
}

func (tr *Tracer) SetClusterID(id string) {
    tr.clusterID.Store(id)
}
```

### 3. Context cancellation check may miss the parent cancel

**File:** `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:65-71` (and identically in `kafka/kafka.go:65-71`)

```go
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)  // shadows outer ctx
defer cancel()
clusterID, err := admin.ClusterID(ctx)
if err != nil {
    if ctx.Err() == context.Canceled {
        return
    }
    ...
}
```

The inner `ctx` (with timeout) shadows the outer `ctx` (with cancel). When the parent context is cancelled (via the stop function), `context.WithTimeout` propagates that cancellation to the child, so `ctx.Err()` on the inner context will indeed be `context.Canceled`. However, if the 2-second timeout fires first, `ctx.Err()` returns `context.DeadlineExceeded`, not `context.Canceled`, which means the timeout case falls through to the warning log. This is arguably the correct behavior (log a warning on timeout, silently exit on explicit cancel), but it is worth noting that `context.Cause(ctx)` could distinguish these more cleanly if the intent ever needs to change.

A clearer alternative that avoids shadowing and makes intent obvious:

```go
timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 2*time.Second)
defer timeoutCancel()
clusterID, err := admin.ClusterID(timeoutCtx)
if err != nil {
    if ctx.Err() != nil {
        // Parent was cancelled (shutdown); exit silently.
        return
    }
    instr.Logger().Warn("failed to fetch Kafka cluster ID: %s", err)
    return
}
```

Checking `ctx.Err()` (the parent) rather than `timeoutCtx.Err()` would correctly differentiate "caller cancelled" from "timed out". The current code checks the *inner* shadowed `ctx.Err()` which is the timeout context -- this means if the timeout fires, it checks `ctx.Err() == context.Canceled` which is false (it's `DeadlineExceeded`), so it logs. If the parent is cancelled, the child also shows `Canceled`, so the silent return happens. The behavior is correct *by accident* of the shadowing, but it would be clearer and more robust without it.

### 4. `admin.Close()` called inside goroutine may conflict with consumer/producer lifecycle

**File:** `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:64` (and `kafka/kafka.go:64`)

```go
defer admin.Close()
```

The admin client is created via `kafka.NewAdminClientFromConsumer(c)` / `kafka.NewAdminClientFromProducer(p)`. In confluent-kafka-go, `NewAdminClientFromConsumer` creates an admin client that shares the underlying librdkafka handle with the consumer. Calling `admin.Close()` on this shared-handle admin client may have side effects depending on the confluent-kafka-go version's reference counting behavior. The sarama integration avoids this issue entirely because `sarama.NewBroker` creates an independent connection.

**Recommendation:** Verify that `admin.Close()` on a `NewAdminClientFrom*` admin client does not prematurely close the shared librdkafka handle. The confluent-kafka-go documentation states that the admin client created this way "does not own the underlying client instance" and `Close()` should be safe, but this is worth a confirming test (e.g., ensure that producing/consuming still works after the admin client is closed).

---

## Nits

### 5. Duplicated `startClusterIDFetch` across kafka.v2 and kafka (v1) packages

**File:** `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:59-81` and `contrib/confluentinc/confluent-kafka-go/kafka/kafka.go:59-81`

The two `startClusterIDFetch` functions are identical. This follows the existing pattern in this contrib where the v1 and v2 packages have separate copies rather than sharing code via `kafkatrace`, but it is worth noting that if the cluster ID fetch logic ever needs to change (e.g., adding retry logic, changing the timeout), it must be updated in both places. Consider whether this helper could live in the shared `kafkatrace` package, accepting an interface for the admin client operations.

### 6. Concurrency test always writes the same value

**File:** `contrib/confluentinc/confluent-kafka-go/kafkatrace/tracer_test.go:79-82`

```go
wg.Go(func() {
    for range numIterations {
        tr.SetClusterID(fmt.Sprintf("cluster-%d", 0))
    }
})
```

The writer always sets `"cluster-0"`. This means the test cannot detect issues like torn reads (two different values being visible), since the written value never changes. Consider varying the value (e.g., `fmt.Sprintf("cluster-%d", i)`) and asserting the reader only ever sees well-formed values. The same issue exists in the IBM/sarama, Shopify/sarama, and segmentio tests (which this test was modeled on), but that does not make it a better test.

### 7. `TestConsumerFunctionalWithClusterID` largely duplicates `TestConsumerFunctional`

**File:** `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka_test.go:146-177` (and `kafka/kafka_test.go`)

The new test is nearly identical to the existing `TestConsumerFunctional` DSM sub-test. The only addition is verifying cluster ID tags are present on both spans. Consider adding the cluster ID assertions directly inside the existing `TestConsumerFunctional` DSM sub-test rather than duplicating the entire flow in a separate test function. This would reduce test maintenance burden and execution time (functional Kafka tests are slow).

### 8. `require.Eventually` in `produceThenConsume` is unconditional but only works with DSM

**File:** `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka_test.go:397` and `kafka/kafka_test.go:382`

```go
require.Eventually(t, func() bool { return p.tracer.ClusterID() != "" }, 5*time.Second, 10*time.Millisecond)
```

This `require.Eventually` is added unconditionally to `produceThenConsume`. If DSM is not enabled, no cluster ID fetch goroutine is started, so `ClusterID()` will always be `""`, and the assertion will timeout after 5 seconds and fail.

Currently this is safe because all callers of `produceThenConsume` pass `WithDataStreams()`. However, this is a latent fragility: if anyone adds a non-DSM test that reuses `produceThenConsume`, it will break unexpectedly. Consider making the wait conditional:

```go
if p.tracer.DSMEnabled() {
    require.Eventually(t, func() bool { return p.tracer.ClusterID() != "" }, 5*time.Second, 10*time.Millisecond)
}
```

### 9. Minor: `TrackKafkaHighWatermarkOffset` doc comment is stale

**File:** `ddtrace/tracer/data_streams.go:77`

```go
// TrackKafkaHighWatermarkOffset should be used in the producer, to track when it produces a message.
```

This says "producer" but it is actually used in the *consumer* to track high watermark offsets. The comment was carried over from the old code and was already incorrect, but this PR touches the function (to wire in cluster), so it would be a good time to fix it.
