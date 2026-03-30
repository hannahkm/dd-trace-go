# Code Review: PR #4470 - feat(dsm): add kafka_cluster_id to confluent-kafka-go

## Overview

This PR adds Kafka cluster ID enrichment to Data Streams Monitoring (DSM) for the confluent-kafka-go instrumentation (both v1 and v2). The cluster ID is fetched asynchronously via the Kafka AdminClient API and plumbed through to span tags, DSM checkpoints, and offset tracking backlogs. The implementation is well-structured overall with proper cancellation, concurrency protection, and graceful degradation on error.

---

## Blocking

### 1. api.txt signatures are incorrect for new public functions

**File:** `ddtrace/tracer/api.txt` (diff lines 616-619)

The api.txt entries for the new public functions have the wrong number of parameters:

```
func TrackKafkaCommitOffsetWithCluster(string, int32, int64)
func TrackKafkaProduceOffsetWithCluster(string, string, int32, int64)
```

The actual signatures in `ddtrace/tracer/data_streams.go` are:

- `TrackKafkaCommitOffsetWithCluster(cluster, group, topic string, partition int32, offset int64)` -- 5 params, api.txt shows 3
- `TrackKafkaProduceOffsetWithCluster(cluster string, topic string, partition int32, offset int64)` -- 4 params, api.txt shows 4 (this one looks correct)

Wait -- re-reading the api.txt diff: `TrackKafkaCommitOffsetWithCluster(string, int32, int64)` only lists 3 types but the real function takes `(string, string, string, int32, int64)`. The api.txt is used for API compatibility tracking, so having wrong signatures is a documentation/tooling problem that could cause confusion in future compatibility checks.

---

## Should Fix

### 2. Double mutex acquisition on ClusterID() in span tagging

**Files:** `contrib/confluentinc/confluent-kafka-go/kafkatrace/consumer.go:70-72`, `producer.go:65-67`

Both `StartConsumeSpan` and `StartProduceSpan` call `tr.ClusterID()` twice in quick succession -- once for the guard and once for the tag value:

```go
if tr.ClusterID() != "" {
    opts = append(opts, tracer.Tag(ext.MessagingKafkaClusterID, tr.ClusterID()))
}
```

Each call acquires and releases an `RLock`. While not a correctness bug (the value is set-once and never cleared), it is a minor inefficiency on every span creation, and creates a theoretical TOCTOU window. Assign the result to a local variable:

```go
if clusterID := tr.ClusterID(); clusterID != "" {
    opts = append(opts, tracer.Tag(ext.MessagingKafkaClusterID, clusterID))
}
```

The same pattern appears in `kafkatrace/dsm.go:53-55` and `dsm.go:73-75` (SetConsumeCheckpoint and SetProduceCheckpoint).

### 3. Context cancellation check uses shadowed `ctx` variable

**Files:** `contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:65-70`, `kafka/kafka.go:65-70`

In `startClusterIDFetch`, the inner goroutine creates a shadowed `ctx`:

```go
func startClusterIDFetch(tr *kafkatrace.Tracer, admin *kafka.AdminClient) func() {
    ctx, cancel := context.WithCancel(context.Background())  // outer ctx
    done := make(chan struct{})
    go func() {
        defer close(done)
        defer admin.Close()
        ctx, cancel := context.WithTimeout(ctx, 2*time.Second)  // shadows outer ctx
        defer cancel()
        clusterID, err := admin.ClusterID(ctx)
        if err != nil {
            if ctx.Err() == context.Canceled {  // checks the INNER (timeout) ctx
                return
            }
```

When the outer cancel is called (from the stop function), the inner `ctx` derived via `WithTimeout` will also be cancelled (since it is a child context). However, `ctx.Err()` on line 69 checks the **inner** (shadowed) context. If the outer cancel fires, the inner context's `Err()` will return `context.Canceled` -- so the logic happens to work in practice. But the intent would be clearer if the error check referenced the parent context directly, or if the variable shadowing were avoided. The current code could also fail to distinguish between a timeout (`context.DeadlineExceeded`) and an external cancellation (`context.Canceled`) if the timeout fires at the same instant as cancellation. This is a readability/maintainability concern, not a likely runtime bug.

### 4. Incorrect docstring on TrackKafkaHighWatermarkOffset (pre-existing but carried forward)

**File:** `ddtrace/tracer/data_streams.go:77`

The docstring says "should be used in the producer, to track when it produces a message" but this function is for tracking high watermark offsets in the **consumer**. The internal `processor.go:702` has the correct docstring. This was pre-existing but is worth fixing while the file is being modified.

### 5. Missing `TrackKafkaHighWatermarkOffsetWithCluster` wrapper for API consistency

**File:** `ddtrace/tracer/data_streams.go:79`

`TrackKafkaCommitOffset` got a `WithCluster` variant and `TrackKafkaProduceOffset` got a `WithCluster` variant, but `TrackKafkaHighWatermarkOffset` was modified in-place to accept `cluster` as its first parameter (previously it was `_` ignored). This is an inconsistency in the public API pattern. The old callers of `TrackKafkaHighWatermarkOffset("", topic, partition, offset)` still work, but the API design is not parallel with the other two functions. Either all three should have `WithCluster` variants (with the original delegating), or none should. Since this function already had the `cluster` param (previously unused), this is a minor API design nit but the inconsistency with the other two functions is notable.

---

## Nits

### 6. Cluster ID test only writes one value despite using `fmt.Sprintf`

**File:** `contrib/confluentinc/confluent-kafka-go/kafkatrace/tracer_test.go:80`

```go
wg.Go(func() {
    for range numIterations {
        tr.SetClusterID(fmt.Sprintf("cluster-%d", 0))
    }
})
```

The writer always sets `"cluster-0"` since the argument is the constant `0`, not the loop variable. This means the test never actually writes different values, making the `assert.Contains(t, id, "cluster-")` check on the reader side trivial. If the intent was to test with varying values (to stress the RWMutex), the loop variable should be used. If the intent was just to verify no data race, the current code is fine but the `fmt.Sprintf` is misleading overhead.

### 7. `closeAsync` slice is nil-initialized and only ever gets 0 or 1 elements

**Files:** `kafka.v2/kafka.go:88`, `kafka/kafka.go:88`

`closeAsync []func()` is used as a slice but only ever has at most one element appended (the cluster ID fetch stop function). A simpler design would be a single `stopClusterIDFetch func()` field, which avoids the slice allocation and makes the intent clearer. The slice design would make sense if more async jobs are planned, but currently it is over-general.

### 8. Test helper `produceThenConsume` uses `require.Eventually` polling for cluster ID

**Files:** `kafka.v2/kafka_test.go:399`, `kafka/kafka_test.go:384`

```go
require.Eventually(t, func() bool { return p.tracer.ClusterID() != "" }, 5*time.Second, 10*time.Millisecond)
```

This is a reasonable approach for integration tests, but the 5-second timeout is quite generous relative to the 2-second fetch timeout. If the fetch fails, the test will hang for 5 seconds before failing rather than failing promptly with a useful error message. A tighter timeout (e.g., 3 seconds) with a descriptive failure message would improve test debugging.

### 9. No test coverage for the cancellation/stop path

**Files:** `kafka.v2/kafka.go:77-80`, `kafka/kafka.go:77-80`

The stop function returned by `startClusterIDFetch` is exercised implicitly via `Close()` in integration tests, but there is no unit test that verifies the cancellation path works correctly -- e.g., that calling stop before the fetch completes causes a clean exit without logging a warning, and that the admin client is closed.

### 10. Backlog tag ordering in tests is fragile

**File:** `internal/datastreams/processor_test.go:594-616`

The `TestKafkaLagWithCluster` test asserts exact tag slices like `[]string{"consumer_group:group1", "partition:1", "topic:topic1", "type:kafka_commit", "kafka_cluster_id:cluster-1"}`. The cluster ID tag is always appended at the end because of the `if key.cluster != ""` guard in the export logic. If the export order ever changes, this test breaks. Using `assert.ElementsMatch` instead of `assert.Equal` for tag comparison would be more robust, though this is admittedly a minor concern.
