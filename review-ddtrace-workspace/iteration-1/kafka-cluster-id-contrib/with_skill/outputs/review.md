# Review: PR #4470 — feat(dsm): add kafka_cluster_id to confluent-kafka-go

## Summary

This PR adds `kafka_cluster_id` enrichment to the confluent-kafka-go integration for Data Streams Monitoring. On consumer/producer creation (when DSM is enabled), it launches a background goroutine to fetch the cluster ID from the Kafka admin API and then attaches it to DSM edge tags, span tags, and backlog metrics. The approach is non-blocking, cancellable on Close, and consistent across both `kafka` and `kafka.v2` packages.

## Blocking

### 1. `api.txt` signature for `TrackKafkaCommitOffsetWithCluster` is wrong

`ddtrace/tracer/api.txt:20` — The api.txt entry reads:

```
func TrackKafkaCommitOffsetWithCluster(string, int32, int64)
```

But the actual function signature in `data_streams.go:54` is:

```go
func TrackKafkaCommitOffsetWithCluster(cluster, group, topic string, partition int32, offset int64)
```

That is 3 string parameters, not 1. The api.txt entry is missing the `group` and `topic` string types. This file is used for API compatibility checking and will produce incorrect results.

### 2. Context cancellation check may miss the outer cancel signal

`contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:65-72` (and identical code in `kafka/kafka.go`) — Inside `startClusterIDFetch`, the inner `ctx` from `context.WithTimeout` shadows the outer `ctx` from `context.WithCancel`. The cancellation check is:

```go
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
clusterID, err := admin.ClusterID(ctx)
if err != nil {
    if ctx.Err() == context.Canceled {
        return
    }
    instr.Logger().Warn(...)
```

When the outer cancel fires (from `Close()`), the inner timeout-derived context will also be cancelled, so `ctx.Err()` will return `context.Canceled` — this works. However, when the 2-second timeout fires on its own, `ctx.Err()` returns `context.DeadlineExceeded`, not `context.Canceled`, so the warning log will fire. This is the correct behavior (timeout is a genuine failure, outer cancel is expected shutdown). But the check is fragile because it relies on the shadowed `ctx` inheriting the cancel signal correctly. Using `errors.Is(err, context.Canceled)` on the error itself would be more robust and idiomatic than checking `ctx.Err()`, and it would still correctly distinguish timeout (logs warning) from shutdown cancel (silent).

### 3. `SetClusterID` and `ClusterID` are exported but internal-only

`contrib/confluentinc/confluent-kafka-go/kafkatrace/tracer.go:43-53` — `SetClusterID` and `ClusterID` are exported methods on an already-exported `Tracer` struct. Per the contrib patterns guidance, functions not intended to be called by users should not be exported. `SetClusterID` is only called from `startClusterIDFetch` (internal plumbing). `ClusterID` is only called internally from other `kafkatrace` methods. These should be unexported (`setClusterID`/`clusterID`) to avoid expanding the public API surface. The `SetClusterID` name also follows the `SetX` convention that could be confused with a public configuration setter.

## Should Fix

### 4. `ClusterID()` called twice in the same code path — unnecessary lock acquisitions

`contrib/confluentinc/confluent-kafka-go/kafkatrace/dsm.go:53-54` and `dsm.go:73-74` — In `SetConsumeCheckpoint` and `SetProduceCheckpoint`, `tr.ClusterID()` is called twice: once for the empty check and once to get the value. Each call acquires the read lock. Capture the value once:

```go
if clusterID := tr.ClusterID(); clusterID != "" {
    edges = append(edges, "kafka_cluster_id:"+clusterID)
}
```

Similarly in `consumer.go:70-71` and `producer.go:65-66`, `tr.ClusterID()` is called twice for the check and the tag value.

### 5. `sync.RWMutex` is heavier than needed for a write-once field

`contrib/confluentinc/confluent-kafka-go/kafkatrace/tracer.go:31-32` — The `clusterID` is written exactly once (from the background goroutine) and read concurrently. Per the concurrency guidance, `atomic.Value` is simpler and sufficient for write-once fields:

```go
type Tracer struct {
    clusterID atomic.Value // stores string, written once
}

func (tr *Tracer) ClusterID() string {
    v, _ := tr.clusterID.Load().(string)
    return v
}
```

This eliminates the mutex entirely and is the pattern reviewers recommend for this exact use case.

### 6. Magic timeout `2*time.Second` should be a named constant

`contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:64` (and `kafka/kafka.go`) — The 2-second timeout for the cluster ID fetch is a magic number. Define a named constant with a comment explaining the choice:

```go
// clusterIDFetchTimeout is the maximum time to wait for the Kafka admin API
// to return the cluster ID. Kept short to avoid delaying close.
const clusterIDFetchTimeout = 2 * time.Second
```

### 7. Warn message does not describe impact

`contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:70` (and `kafka/kafka.go`) — The warning `"failed to fetch Kafka cluster ID: %s"` doesn't explain what the user loses. Per the contrib patterns guidance, error messages should describe impact:

```go
instr.Logger().Warn("failed to fetch Kafka cluster ID; kafka_cluster_id will be missing from DSM metrics: %s", err)
```

The admin client creation failure messages (`kafka.go:102`, `kafka.go:222`) are better — they say "not adding cluster_id tags" — but still could mention DSM specifically.

### 8. Duplicate `startClusterIDFetch` across `kafka` and `kafka.v2`

`contrib/confluentinc/confluent-kafka-go/kafka/kafka.go` and `kafka.v2/kafka.go` — The `startClusterIDFetch` function is copy-pasted identically between the two packages (only the `kafka.AdminClient` import differs). This is a known constraint of the v1/v2 package split, but worth noting: any bug fix to this function must be applied in both places.

### 9. Missing `checklocks` annotation on `clusterID` field

`contrib/confluentinc/confluent-kafka-go/kafkatrace/tracer.go:31` — The `clusterID` field is guarded by `clusterIDMu`, but there is no `// +checklocks:clusterIDMu` annotation. The repo uses the `checklocks` static analyzer to verify lock discipline. (This is moot if switching to `atomic.Value` per finding #5.)

## Nits

### 10. Godoc comments missing on `ClusterID()` and `SetClusterID()`

`contrib/confluentinc/confluent-kafka-go/kafkatrace/tracer.go:43,49` — These exported methods have no godoc comments. Even if they should be unexported (per #3), they should have comments describing what they do.

### 11. `TestClusterIDConcurrency` writer always sets the same value

`contrib/confluentinc/confluent-kafka-go/kafkatrace/tracer_test.go:80` — The writer goroutine always sets `fmt.Sprintf("cluster-%d", 0)` which is always `"cluster-0"`. The `0` appears to be a leftover from a loop that was removed. This doesn't affect test correctness (it still validates concurrent access), but the constant index is misleading.

### 12. `TestConsumerFunctionalWithClusterID` largely duplicates `TestConsumerFunctional`

`contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka_test.go:146` — The new test has significant overlap with the existing `TestConsumerFunctional` which was also updated to check cluster IDs. Consider whether both tests are needed, or if the existing test already covers the cluster ID assertions after the modifications.
