# Review: PR #4470 -- feat(dsm): add kafka_cluster_id to confluent-kafka-go

## Summary

This PR adds Kafka cluster ID enrichment to the confluent-kafka-go integration for Data Streams Monitoring (DSM). When DSM is enabled, it asynchronously fetches the cluster ID via an admin client on consumer/producer creation and uses it to tag spans and DSM edge tags/backlogs. The approach is non-blocking with a 2-second timeout, cancellable on Close, and follows the same pattern already established in the segmentio/kafka-go integration.

The overall design is solid and consistent with the existing cluster ID implementations in other contrib integrations (Shopify/sarama, IBM/sarama, segmentio/kafka-go).

## Blocking

1. **`api.txt` signature for `TrackKafkaCommitOffsetWithCluster` is wrong** (`ddtrace/tracer/api.txt`).

   The entry reads `func TrackKafkaCommitOffsetWithCluster(string, int32, int64)` but the actual function signature is `func TrackKafkaCommitOffsetWithCluster(cluster, group, topic string, partition int32, offset int64)` which in api.txt notation should be `func TrackKafkaCommitOffsetWithCluster(string, string, string, int32, int64)`. The existing `TrackKafkaCommitOffset(string, int32, int64)` works because Go groups `(group, topic string)` into one type token, yielding `(string, int32, int64)`. But `TrackKafkaCommitOffsetWithCluster` has three string params (`cluster, group, topic string`), so it needs three distinct `string` entries or a grouped representation: `(string, string, int32, int64)` at minimum. As written, the api.txt will mismatch what automated API stability tools generate, which will likely break the `apidiff` CI check. Verify by regenerating the api.txt entry.

2. **Cancellation check uses `context.Canceled` but could also see `context.DeadlineExceeded`** (`contrib/confluentinc/confluent-kafka-go/kafka.v2/kafka.go:69`, `contrib/confluentinc/confluent-kafka-go/kafka/kafka.go:69`).

   The `startClusterIDFetch` goroutine checks `ctx.Err() == context.Canceled` to suppress the log on expected cancellation. However, `ctx` at that point is the *inner* `WithTimeout` context, not the outer `WithCancel` one (the inner `ctx` shadows the outer). When the parent cancel fires, the inner context's `Err()` will still return `context.Canceled`, so the current logic works correctly for the Close path. But if the 2-second timeout expires (a legitimate expected failure), `ctx.Err()` returns `context.DeadlineExceeded`, and the code logs a `Warn` -- which is arguably noisy for an expected condition (slow broker). Consider also suppressing `context.DeadlineExceeded` or using `errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)` to only warn on truly unexpected errors. Alternatively, check `ctx.Err() != nil` to suppress all context-related errors and only warn on broker-level failures. Note: the segmentio integration has the same pattern, so this is consistent but potentially noisy in both places.

## Should fix

1. **Double lock acquisition in `ClusterID()` calls** (`kafkatrace/consumer.go:70-71`, `kafkatrace/producer.go:65-66`, `kafkatrace/dsm.go:53-54`, `kafkatrace/dsm.go:73-74`).

   Each call site does `if tr.ClusterID() != "" { ... tr.ClusterID() ... }` which acquires the read lock twice. While this is not a correctness bug (the value is write-once and the RWMutex is fine here), the concurrency reference recommends against acquiring the same lock multiple times when a single acquisition would suffice. A simple local variable eliminates the redundant lock:
   ```go
   if cid := tr.ClusterID(); cid != "" {
       opts = append(opts, tracer.Tag(ext.MessagingKafkaClusterID, cid))
   }
   ```
   This is consistent with how `bootstrapServers` is read once in the same functions. Note: the segmentio/kafka-go integration has the same pattern, so if this is changed, both should be updated for consistency.

2. **`SetClusterID` and `ClusterID` are exported but only called internally** (`kafkatrace/tracer.go:43-53`).

   The `SetClusterID` method is only called from `startClusterIDFetch` within the same contrib package. The `ClusterID` method is called from `kafkatrace` (internal) and test code. Per the contrib patterns guidance, functions that won't be called by users should not be exported. However, looking at the precedent set by Shopify/sarama (`option.go:39`), IBM/sarama (`option.go:39`), and segmentio/kafka-go (`tracer.go:110`), all of these integrations also export `SetClusterID`. So this is consistent with existing practice. Still worth considering whether these could be unexported if they are truly internal-only, but this is not blocking given the established pattern.

3. **Concurrency reference suggests `atomic.Value` for write-once fields** (`kafkatrace/tracer.go:31-32`).

   The `clusterID` is set once from a goroutine and then only read. The concurrency reference specifically calls out `atomic.Value` as preferred over `sync.RWMutex` for this pattern. That said, segmentio/kafka-go uses `sync.RWMutex` for the same field, so the PR is consistent with existing integrations. An `atomic.Value` would be simpler:
   ```go
   clusterID atomic.Value // stores string, written once

   func (tr *Tracer) ClusterID() string {
       v, _ := tr.clusterID.Load().(string)
       return v
   }
   func (tr *Tracer) SetClusterID(id string) {
       tr.clusterID.Store(id)
   }
   ```

4. **Magic timeout value `2*time.Second`** (`kafka.v2/kafka.go:65`, `kafka/kafka.go:65`).

   The 2-second timeout for the cluster ID fetch is an inline magic number. Per the style guide, timeout values should be named constants with a comment explaining the choice. Define something like:
   ```go
   const clusterIDFetchTimeout = 2 * time.Second // keep short to avoid blocking user startup
   ```

## Nits

1. **Godoc missing on `ClusterID` and `SetClusterID`** (`kafkatrace/tracer.go:43, 49`).

   Both exported methods lack godoc comments. Even if these are semi-internal, exported symbols should have godoc per Go convention. A brief comment like `// ClusterID returns the Kafka cluster ID, or empty string if not yet fetched.` would suffice.

2. **`startClusterIDFetch` is duplicated identically** across `kafka.v2/kafka.go` and `kafka/kafka.go`.

   The function body is character-for-character identical between the v1 and v2 packages. This is a known pattern in this repo (the two packages mirror each other for the two confluent-kafka-go major versions), so this is expected and not actionable -- just noting it for awareness.

3. **Test `TestClusterIDConcurrency` writer uses hardcoded `cluster-0`** (`kafkatrace/tracer_test.go:77`).

   The writer goroutine always writes `fmt.Sprintf("cluster-%d", 0)` which is always `"cluster-0"`. The `numIterations` loop and `fmt.Sprintf` formatting suggest this may have intended to vary the value (e.g., using the loop index). As-is, the test still validates concurrent read/write safety, so this is fine functionally.

4. **Comment on `TrackKafkaHighWatermarkOffset` is stale** (`ddtrace/tracer/data_streams.go:77`).

   The PR removed the comment `// The first argument is the Kafka cluster ID, and will be used later.` from the processor level, but the public-facing `TrackKafkaHighWatermarkOffset` at `data_streams.go:77` still says `// TrackKafkaHighWatermarkOffset should be used in the producer, to track when it produces a message.` -- this says "producer" but it is used in the consumer to track high watermark offsets. This predates the PR but is worth fixing while touching this file.
