# Code Review: PR #4470 - feat(dsm): add kafka_cluster_id to confluent-kafka-go

**PR:** https://github.com/DataDog/dd-trace-go/pull/4470
**Author:** robcarlan-datadog
**Status:** MERGED
**Summary:** Adds `kafka_cluster_id` enrichment to Data Streams Monitoring for the confluent-kafka-go integration (v1 and v2). The cluster ID is fetched asynchronously via the Kafka Admin API and propagated to DSM checkpoints, backlog tags, and span tags.

---

## Blocking

### 1. api.txt has wrong function signatures for the new `WithCluster` functions

**File:** `ddtrace/tracer/api.txt:19,23`

The api.txt entries for the new functions are missing parameters:

```
func TrackKafkaCommitOffsetWithCluster(string, int32, int64)
func TrackKafkaProduceOffsetWithCluster(string, string, int32, int64)
```

But the actual Go signatures in `ddtrace/tracer/data_streams.go` are:

```go
func TrackKafkaCommitOffsetWithCluster(cluster, group, topic string, partition int32, offset int64)
func TrackKafkaProduceOffsetWithCluster(cluster string, topic string, partition int32, offset int64)
```

`TrackKafkaCommitOffsetWithCluster` should list 3 string params (cluster, group, topic) before the int32 and int64, but the api.txt only shows `(string, int32, int64)` -- that is 3 parameters instead of 5. Similarly, `TrackKafkaProduceOffsetWithCluster` shows `(string, string, int32, int64)` -- 4 parameters instead of 4, which happens to be correct in count but was likely generated from a stale state given the commit history showing reordered parameters. This api.txt file appears auto-generated but should be verified to match the final function signatures, as it is used for API stability tracking.

### 2. `TrackKafkaHighWatermarkOffset` doc comment is wrong

**File:** `ddtrace/tracer/data_streams.go:77-78`

```go
// TrackKafkaHighWatermarkOffset should be used in the producer, to track when it produces a message.
// if used together with TrackKafkaCommitOffset it can generate a Kafka lag in seconds metric.
```

This comment is copied from `TrackKafkaProduceOffset`. The high watermark offset is tracked by the **consumer**, not the producer, and represents the highest offset available in the partition -- not a produce event. The comment should say something like "should be used in the consumer, to track the high watermark offset of each partition."

---

## Should Fix

### 3. Double acquisition of RWMutex when reading ClusterID in span creation hot paths

**Files:**
- `contrib/confluentinc/confluent-kafka-go/kafkatrace/consumer.go:70-72`
- `contrib/confluentinc/confluent-kafka-go/kafkatrace/producer.go:65-67`

```go
if tr.ClusterID() != "" {
    opts = append(opts, tracer.Tag(ext.MessagingKafkaClusterID, tr.ClusterID()))
}
```

`tr.ClusterID()` acquires the read lock twice on every span creation -- once for the check and once for the value. This is on the hot path for every produce and consume operation. The fix is trivial: read the value once into a local variable.

```go
if cid := tr.ClusterID(); cid != "" {
    opts = append(opts, tracer.Tag(ext.MessagingKafkaClusterID, cid))
}
```

### 4. Same double-lock issue in DSM checkpoint paths

**File:** `contrib/confluentinc/confluent-kafka-go/kafkatrace/dsm.go:53-55,72-74`

```go
if tr.ClusterID() != "" {
    edges = append(edges, "kafka_cluster_id:"+tr.ClusterID())
}
```

Same pattern in both `SetConsumeCheckpoint` and `SetProduceCheckpoint`. Should read once into a local variable.

### 5. Context cancellation check in `startClusterIDFetch` has a race with the timeout context

**Files:**
- `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:65-73`
- `contrib/confluentinc/confluent-kafka-go/kafka/kafka.go:65-73`

```go
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
clusterID, err := admin.ClusterID(ctx)
if err != nil {
    if ctx.Err() == context.Canceled {
        return
    }
    instr.Logger().Warn("failed to fetch Kafka cluster ID: %s", err)
    return
}
```

The inner `ctx` is derived from both the parent cancel context AND a 2-second timeout. When the timeout fires, `ctx.Err()` returns `context.DeadlineExceeded`, not `context.Canceled`, so the warning log fires correctly for timeouts. However, if the parent context is cancelled (via `Close()`), the inner `ctx.Err()` could be either `context.Canceled` or `context.DeadlineExceeded` depending on timing. It would be more robust to check the parent context for cancellation:

```go
if err != nil {
    if parentCtx.Err() == context.Canceled {
        return  // Close() was called, expected cancellation
    }
    instr.Logger().Warn(...)
}
```

This was also flagged by the Codex automated review as a noisy false-positive warning path during shutdown. The current code on the merged branch does check `ctx.Err() == context.Canceled`, which partially addresses this but is fragile because `ctx` is the timeout-wrapped child. Checking the parent cancellation context would be unambiguous.

### 6. `startClusterIDFetch` is duplicated verbatim between kafka v1 and v2 packages

**Files:**
- `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:59-81`
- `contrib/confluentinc/confluent-kafka-go/kafka/kafka.go:59-81`

The function is identical in both packages (same logic, same structure, same comments). The only difference is the import of `kafka` (v1 vs v2). Given that `kafkatrace` already serves as the shared package between v1 and v2, consider whether a generic helper or a shared function that accepts an interface (with `ClusterID(ctx) (string, error)` and `Close()` methods) could deduplicate this. This was also called out in reviewer feedback about keeping kafkatrace's surface area minimal.

### 7. No test for timeout behavior of cluster ID fetch

There is no test verifying that when the cluster ID fetch times out (e.g., broker unreachable), the consumer/producer still functions correctly and `ClusterID()` returns empty string gracefully. The integration tests rely on `require.Eventually` waiting for the cluster ID to become available, but there is no test for the failure/timeout path. Given the 2-second timeout and the async nature, a unit test mocking a slow or unreachable admin client would be valuable.

---

## Nits

### 8. Inconsistent `Sprintf` usage for tag formatting in backlog export

**File:** `internal/datastreams/processor.go:124-146`

Some tags use `fmt.Sprintf("kafka_cluster_id:%s", key.cluster)` while the edge tag construction in `kafkatrace/dsm.go` uses string concatenation `"kafka_cluster_id:"+tr.ClusterID()`. The processor file uses `Sprintf` for all existing tags (partition, topic, consumer_group) which is consistent internally, but is slightly heavier than concatenation. Not a real issue, just noting the inconsistency between the two files.

### 9. `TestClusterIDConcurrency` writer always writes the same value

**File:** `contrib/confluentinc/confluent-kafka-go/kafkatrace/tracer_test.go:80`

```go
tr.SetClusterID(fmt.Sprintf("cluster-%d", 0))
```

The writer goroutine always writes `"cluster-0"` (the format arg is always `0`). For a more meaningful concurrency test, it could write varying values (e.g., `fmt.Sprintf("cluster-%d", i)`) to verify readers see consistent (non-torn) values under concurrent writes.

### 10. `closeAsync` field initialized as nil, only populated via `append`

**Files:**
- `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:88`
- `contrib/confluentinc/confluent-kafka-go/kafka/kafka.go:88`

```go
closeAsync []func() // async jobs to cancel and wait for on Close
```

The field is never pre-initialized and only ever has at most one element appended. Using `append` on a nil slice to a `[]func{}` works fine in Go, but the slice abstraction (supporting multiple async jobs) is over-engineered for the current single use case. A single `stopFn func()` field would be simpler and more obvious, though the current design is forward-compatible if more async jobs are added later.

### 11. Missing `t.Parallel()` on new test functions

**Files:**
- `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka_test.go:146` (`TestConsumerFunctionalWithClusterID`)
- `contrib/confluentinc/confluent-kafka-go/kafka/kafka_test.go:162` (`TestConsumerFunctionalWithClusterID`)
- `contrib/confluentinc/confluent-kafka-go/kafkatrace/tracer_test.go:70` (`TestClusterIDConcurrency`)
- `internal/datastreams/processor_test.go:585` (`TestKafkaLagWithCluster`)

New test functions do not call `t.Parallel()`. If the existing tests in these files use `t.Parallel()`, the new ones should follow suit for consistency and faster CI.

### 12. The `closeAsync` loop in `Close()` runs stop functions sequentially

**Files:**
- `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:117-119`
- `contrib/confluentinc/confluent-kafka-go/kafka/kafka.go:117-119`

```go
for _, stopAsync := range c.closeAsync {
    stopAsync()
}
```

If multiple async jobs were registered, they would be stopped sequentially (each one cancels then waits). For the current single-job case this is fine, but if the `closeAsync` slice grows, cancelling all first and then waiting would be faster. Minor since only one job exists today.
