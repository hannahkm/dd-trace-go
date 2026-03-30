# Review: PR #4470 — feat(dsm): add kafka_cluster_id to confluent-kafka-go

## Summary

This PR adds Kafka cluster ID enrichment to the confluent-kafka-go contrib integration for Data Streams Monitoring. It launches an async goroutine on consumer/producer creation to fetch the cluster ID via the AdminClient API, then plumbs that ID through DSM checkpoints, offset tracking, and span tags. The implementation is duplicated across kafka (v1) and kafka.v2 packages. The design is sound: async fetch avoids blocking user code, cancellation on Close prevents goroutine leaks, and DSM guards prevent unnecessary work when DSM is disabled.

---

## Blocking

### 1. `api.txt` signature for `TrackKafkaCommitOffsetWithCluster` is wrong

`ddtrace/tracer/api.txt` (from the diff):
```
func TrackKafkaCommitOffsetWithCluster(string, int32, int64)
```

The actual function signature has 5 parameters: `(cluster, group, topic string, partition int32, offset int64)`. The api.txt entry is missing the `group` and `topic` string parameters. This will cause the API surface checker to fail or silently accept a wrong contract. `TrackKafkaProduceOffsetWithCluster` in api.txt shows `(string, string, int32, int64)` which is correct (4 params), so only the commit variant is broken.

### 2. Double call to `ClusterID()` acquires the RWMutex twice per span

In `consumer.go:70-72` and `producer.go:65-67`:
```go
if tr.ClusterID() != "" {
    opts = append(opts, tracer.Tag(ext.MessagingKafkaClusterID, tr.ClusterID()))
}
```

Each call to `ClusterID()` acquires and releases the `RWMutex`. On the hot path of span creation (every produce/consume), this is two lock acquisitions where one suffices. This is the exact pattern called out in the concurrency guidance ("We're now getting the locking twice"). Store the result in a local variable:

```go
if cid := tr.ClusterID(); cid != "" {
    opts = append(opts, tracer.Tag(ext.MessagingKafkaClusterID, cid))
}
```

The same double-call pattern appears in `dsm.go:53-54` (`SetConsumeCheckpoint`) and `dsm.go:73-74` (`SetProduceCheckpoint`). These are also on the per-message hot path.

### 3. `sync.RWMutex` for a write-once field -- consider `atomic.Value`

Per the concurrency reference: "When a field is set once from a goroutine and read concurrently, reviewers suggest `atomic.Value` over `sync.RWMutex` -- it's simpler and sufficient." The `clusterID` field is written exactly once (from the async fetch goroutine) and read on every produce/consume span. `atomic.Value` would eliminate all mutex contention on reads and simplify the code:

```go
type Tracer struct {
    clusterID atomic.Value // stores string, written once
}

func (tr *Tracer) ClusterID() string {
    v, _ := tr.clusterID.Load().(string)
    return v
}

func (tr *Tracer) SetClusterID(id string) {
    tr.clusterID.Store(id)
}
```

This is a direct pattern match from real review feedback on this repo.

---

## Should fix

### 4. Warn message on cluster ID fetch does not describe impact

In `startClusterIDFetch` (both v1 and v2):
```go
instr.Logger().Warn("failed to fetch Kafka cluster ID: %s", err)
```

Per the universal checklist and contrib patterns reference, error messages should explain what the user loses. A better message:

```go
instr.Logger().Warn("failed to fetch Kafka cluster ID; kafka_cluster_id will be missing from DSM metrics and span tags: %s", err)
```

The same applies to the admin client creation failure message, which is better (`"failed to create admin client for cluster ID, not adding cluster_id tags: %s"`) but could also mention DSM metrics.

### 5. `startClusterIDFetch` is duplicated identically across kafka v1 and v2

The function `startClusterIDFetch` is copy-pasted between `kafka/kafka.go` and `kafka.v2/kafka.go` -- the implementation is character-for-character identical. The contrib patterns reference says to "extract shared/duplicated logic" and "follow the existing pattern" across similar integrations. This function could live in `kafkatrace/` (which is already shared between v1 and v2), parameterized by an interface for the admin client. The `kafkatrace` package already holds all the shared Tracer logic. However, since the `AdminClient` types differ between v1 (`kafka.AdminClient`) and v2 (`kafka.AdminClient` from different import paths), this may require a small interface. If that's too much churn for this PR, at minimum add a comment noting the duplication.

### 6. Cancellation check may miss timeout errors

In `startClusterIDFetch`, the error handling checks:
```go
if ctx.Err() == context.Canceled {
    return
}
instr.Logger().Warn("failed to fetch Kafka cluster ID: %s", err)
```

If the 2-second `WithTimeout` fires (a deadline exceeded, not a cancellation), the code will log a warning. This is probably fine. But if the outer cancel fires *while* the timeout context is also expired, `ctx.Err()` could return `context.DeadlineExceeded` (from the timeout child) rather than `context.Canceled` (from the parent). The check should use `errors.Is(err, context.Canceled)` on the returned error to be robust, or also check for `context.DeadlineExceeded` since a timeout is equally expected/non-actionable:

```go
if ctx.Err() != nil {
    return // cancelled or timed out -- either way, nothing to warn about
}
```

A timeout on the cluster ID fetch is arguably expected behavior (e.g., broker unreachable) and not something an operator can act on from a warning log.

### 7. `TestClusterIDConcurrency` writer only writes one value

In `tracer_test.go:78-82`:
```go
wg.Go(func() {
    for range numIterations {
        tr.SetClusterID(fmt.Sprintf("cluster-%d", 0))
    }
})
```

`fmt.Sprintf("cluster-%d", 0)` always produces `"cluster-0"`. This doesn't actually test interleaving of different values -- the reader assertion `assert.Contains(t, id, "cluster-")` would always pass. To meaningfully exercise the race detector, the writer should vary the value (e.g., use the loop index). Also, `fmt` is imported solely for this constant string -- `tr.SetClusterID("cluster-0")` would be simpler.

---

## Nits

### 8. Shadowed `ctx` and `cancel` in `startClusterIDFetch`

Inside the goroutine:
```go
ctx, cancel := context.WithCancel(context.Background())  // outer
go func() {
    ctx, cancel := context.WithTimeout(ctx, 2*time.Second)  // shadows both
    defer cancel()
```

The inner `ctx, cancel :=` shadows the outer variables. While this works correctly (the inner `cancel` is deferred, and the outer `cancel` is only used in the stop function), shadowed context variables are a common source of confusion in reviews. Consider naming them differently (e.g., `fetchCtx` for the timeout context) for clarity.

### 9. Magic number `2*time.Second` for cluster ID fetch timeout

The 2-second timeout in `startClusterIDFetch` is an inline magic number. Per the style guide, timeout values should be named constants with a comment explaining the choice:

```go
const clusterIDFetchTimeout = 2 * time.Second // short timeout to avoid delaying shutdown
```

### 10. `wg.Go` usage requires Go 1.24+

`TestClusterIDConcurrency` uses `sync.WaitGroup.Go()` which was added in Go 1.24. This is fine if the repo's minimum Go version is >= 1.24, but worth noting for compatibility awareness.
