# Review: PR #4470 — feat(dsm): add kafka_cluster_id to confluent-kafka-go

## Summary

This PR adds `kafka_cluster_id` enrichment to DSM (Data Streams Monitoring) for the confluent-kafka-go integration. On consumer/producer creation, it launches an async goroutine to fetch the cluster ID via the Kafka admin API, then uses it to tag spans and DSM edge tags/backlogs. The async fetch is cancellable on `Close()` to avoid blocking shutdown.

The overall design is solid and follows established patterns in the repo (async fetch with cancellation, `closeAsync` slice pattern, DSM gating). The code is well-structured with good test coverage including a concurrency test. Below are the findings.

---

## Blocking

### 1. `api.txt` signatures are wrong for `TrackKafkaCommitOffsetWithCluster`

The diff adds this to `ddtrace/tracer/api.txt`:
```
func TrackKafkaCommitOffsetWithCluster(string, int32, int64)
```

But the actual function signature at `ddtrace/tracer/data_streams.go:54` is:
```go
func TrackKafkaCommitOffsetWithCluster(cluster, group, topic string, partition int32, offset int64)
```

That's 5 parameters (3 strings, int32, int64), so the api.txt entry should be `(string, string, string, int32, int64)`. The current entry drops two string parameters. This will cause the API stability checker to report incorrect surface area.

(Note: the existing `TrackKafkaCommitOffset(string, int32, int64)` entry also appears wrong -- it should be `(string, string, int32, int64)` since the actual signature is `(group, topic string, partition int32, offset int64)` -- but that's a pre-existing issue.)

### 2. Cancellation check uses wrong context -- outer cancel never detected

In `startClusterIDFetch` (both `kafka.go` and `kafka.v2/kafka.go`):

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
            if ctx.Err() == context.Canceled {  // checks inner ctx
                return
            }
            instr.Logger().Warn("failed to fetch Kafka cluster ID: %s", err)
            return
        }
        tr.SetClusterID(clusterID)
    }()
    return func() {
        cancel()  // cancels outer ctx
        <-done
    }
}
```

When the stop function calls `cancel()` on the outer context, the inner `WithTimeout` context (derived from the outer) will also be cancelled. However, the error check `ctx.Err() == context.Canceled` checks the **inner** (shadowed) `ctx`. In practice this still works because `WithTimeout` propagates parent cancellation, so the inner ctx will also report `context.Canceled`. But there's a subtle issue: if the `WithTimeout` expires (2s deadline) *at the same time* as the outer cancel, `ctx.Err()` could return `context.DeadlineExceeded` instead of `context.Canceled`, causing the expected-cancellation case to fall through to the warning log. This is a minor correctness issue -- the real concern is the shadowed variable makes the code harder to reason about. Consider checking the error value itself with `errors.Is(err, context.Canceled)` (which is also the idiomatic Go pattern, as used elsewhere in this repo -- see `contrib/haproxy/`, `contrib/envoyproxy/`, `contrib/google.golang.org/grpc/`).

---

## Should Fix

### 3. Error messages should describe impact, not just the failure

`contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:72` (and the v1 equivalent):
```go
instr.Logger().Warn("failed to fetch Kafka cluster ID: %s", err)
```

Per review conventions, this should explain what the user loses. Something like:
```go
instr.Logger().Warn("failed to fetch Kafka cluster ID; kafka_cluster_id will be missing from DSM metrics: %s", err)
```

The admin client creation failure at line 66 already has good impact context ("not adding cluster_id tags"), but the fetch failure inside the goroutine does not.

### 4. Double lock acquisition for `ClusterID()` in span creation

In `kafkatrace/consumer.go:70-71` and `kafkatrace/producer.go:65-66`:
```go
if tr.ClusterID() != "" {
    opts = append(opts, tracer.Tag(ext.MessagingKafkaClusterID, tr.ClusterID()))
}
```

Each call to `ClusterID()` acquires the `RWMutex`. This acquires the lock twice on every span when cluster ID is set. Since spans are created on every message, this is a hot path. Read the value once into a local variable:

```go
if cid := tr.ClusterID(); cid != "" {
    opts = append(opts, tracer.Tag(ext.MessagingKafkaClusterID, cid))
}
```

The same double-call pattern appears in `kafkatrace/dsm.go:53-54` and `dsm.go:73-74` for the edge tag appending.

### 5. Consider `atomic.Value` instead of `sync.RWMutex` for write-once field

Per the concurrency reference, `atomic.Value` is preferred over `sync.RWMutex` for fields that are written once and read concurrently. `clusterID` is set once from the async goroutine and then read on every span. `atomic.Value` would be simpler and avoid lock contention on the hot path:

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

This would also eliminate the double-lock concern in finding #4.

### 6. `SetClusterID` and `ClusterID` are exported but only used internally

`kafkatrace/tracer.go` exports `SetClusterID` and `ClusterID` as public methods on the `Tracer` struct. `SetClusterID` is only called from the `startClusterIDFetch` function within the contrib package. Per the contrib patterns reference, functions that won't be called by users should not be exported. Consider making these unexported (`setClusterID` / `clusterID`), or documenting why they need to be public.

Note: `Tracer` itself is already exported and has public fields (like `PrevSpan`), so this is a "should fix" rather than blocking -- but it adds to the public API surface unnecessarily.

### 7. Magic timeout value `2*time.Second`

The 2-second timeout for the cluster ID fetch in `startClusterIDFetch` is a hardcoded magic number. Per style conventions, this should be a named constant with a comment explaining the choice:

```go
// clusterIDFetchTimeout is the maximum time to wait for the Kafka admin API
// to return the cluster ID. Kept short to avoid delaying observability enrichment
// while being long enough for most broker responses.
const clusterIDFetchTimeout = 2 * time.Second
```

---

## Nits

### 8. Shadowed variable names in `startClusterIDFetch`

The inner `ctx, cancel :=` shadows the outer `ctx, cancel` on the very next line. This compiles fine but makes the code harder to follow. Consider naming the inner pair differently (e.g., `timeoutCtx, timeoutCancel`).

### 9. `TestConsumerFunctionalWithClusterID` largely duplicates `TestConsumerFunctional`

The new test in `kafka.v2/kafka_test.go:146` covers the same produce-then-consume flow as the existing test, with the only addition being cluster ID assertions. Since the existing `TestConsumerFunctional` was also updated to assert cluster ID, consider whether the new test adds enough distinct coverage to justify the duplication, or whether the cluster ID assertions in the existing test are sufficient.

### 10. Minor: `fmt.Sprintf("cluster-%d", 0)` in concurrency test

In `kafkatrace/tracer_test.go:77`:
```go
tr.SetClusterID(fmt.Sprintf("cluster-%d", 0))
```

The format argument is always `0`, so this is always `"cluster-0"`. If the intent was to vary the value per iteration, the loop variable should be used. If the intent was a fixed value, a string literal `"cluster-0"` would be clearer without the `fmt` import.
