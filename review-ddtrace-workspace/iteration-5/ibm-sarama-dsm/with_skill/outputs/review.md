# Review: PR #4486 — feat(dsm): add kafka_cluster_id to IBM/sarama integration

## Summary

This PR adds `kafka_cluster_id` to the IBM/sarama (and Shopify/sarama) DSM integrations: edge tags, offset tracking, and span tags. It also extracts the cluster ID cache and async fetcher from `kafkatrace` into a shared `instrumentation/kafkaclusterid` package, updates the confluent-kafka-go integration to use the new shared `Fetcher` type (replacing the hand-rolled `clusterID` + `sync.RWMutex` + channel pattern), and changes `Close()` from `WaitForClusterID` (blocking until fetch completes) to `StopClusterIDFetch` (cancel + wait, instant).

## Reference files consulted

- style-and-idioms.md (always)
- contrib-patterns.md (contrib integration patterns, DSM, consistency across integrations)
- concurrency.md (async goroutines, cancellation, shared state)

## Findings

### Blocking

1. **`Fetcher.FetchAsync` has a data race on `cancel` and `ready` fields** (`instrumentation/kafkaclusterid/fetcher.go:44-53`). The `cancel` and `ready` fields are assigned directly in `FetchAsync` without holding the mutex, and read in `Stop` and `Wait` without synchronization. If `FetchAsync` is called from one goroutine while `Stop` is called from another (e.g., rapid init/shutdown), there is a race on these fields. The `id` field is properly guarded by `mu`, but `cancel` and `ready` are not. Consider either guarding them under `mu`, or documenting that `FetchAsync` must be called before any concurrent `Stop`/`Wait` (and ensuring call sites satisfy that contract). The current confluent-kafka-go usage appears safe (FetchAsync in constructor, Stop in Close), but the Fetcher is exported and could be misused.

2. **`fetchClusterID` in IBM/sarama and Shopify/sarama is duplicated almost line-for-line** (`contrib/IBM/sarama/option.go:125-154`, `contrib/Shopify/sarama/option.go:107-136`). Per the contrib-patterns reference on consistency across similar integrations: these two `fetchClusterID` functions are nearly identical (they differ only in the log prefix: `"contrib/IBM/sarama"` vs `"contrib/Shopify/sarama"`). The whole point of extracting `kafkaclusterid` into a shared package was to centralize logic. The broker metadata fetch should also be extracted — either into `kafkaclusterid` (with a generic broker interface) or into a shared sarama helper since both packages import the same `sarama` library type. The `WithBrokers` option function bodies are also duplicated.

### Should fix

1. **Error messages don't describe impact** (`contrib/IBM/sarama/option.go:139,146`). The warnings `"failed to open broker for cluster ID: %s"` and `"failed to fetch Kafka cluster ID: %s"` describe what failed but not the consequence. Per the universal checklist: explain what the user loses, e.g., `"failed to open broker for cluster ID; kafka_cluster_id will be missing from DSM edge tags: %s"`. Same issue in the Shopify/sarama copy.

2. **`WithBrokers` only connects to `addrs[0]`** (`contrib/IBM/sarama/option.go:137`). The function accepts a list of broker addresses but only opens a connection to the first one. If that broker is down, the cluster ID fetch fails even though other brokers are available. The confluent-kafka-go integration uses the admin client which handles failover internally. Consider trying brokers in order until one succeeds, or at minimum documenting that only the first broker is used.

3. **Double cache lookup in `fetchClusterID`** (`contrib/IBM/sarama/option.go:126-132`). `WithBrokers` already checks the cache and only calls `FetchAsync` on a miss. Inside `FetchAsync`'s callback, `fetchClusterID` checks the cache again. This double-check is a defensive pattern (the cache could be populated by another goroutine between the check and the async fetch), so it is valid. However, the `NormalizeBootstrapServersList` call is also duplicated between `WithBrokers` and `fetchClusterID`. Consider passing the pre-computed key into `fetchClusterID` to avoid re-normalization.

4. **`cluster_id.go` wrapper functions in kafkatrace are thin aliases** (`contrib/confluentinc/confluent-kafka-go/kafkatrace/cluster_id.go:11-28`). The new `cluster_id.go` file creates four exported functions that are pure pass-throughs to `kafkaclusterid`. Per the style-and-idioms reference on unnecessary aliases: "Only create aliases when there's a genuine need." If these exist to maintain backward compatibility for external callers of the `kafkatrace` package, they are justified. If they are only used internally within the `confluent-kafka-go` contrib, they add unnecessary indirection and should be replaced with direct `kafkaclusterid` imports.

5. **`ResetCache` uses `cache = sync.Map{}` which is a non-atomic replacement of a global** (`instrumentation/kafkaclusterid/cache.go:67-68`). This is the same pattern that was in the old code. Since it is test-only, it is acceptable, but a concurrent `Load` or `Store` on the old `sync.Map` while `ResetCache` replaces the variable is technically a race. `sync.Map` methods are goroutine-safe, but replacing the entire variable is not. Consider using `cache.Range` + `cache.Delete` for a safe clear, or accept this as a test-only limitation.

### Nits

1. **`Fetcher.ClusterIDFetcher` is exported in the `Tracer` struct** (`contrib/confluentinc/confluent-kafka-go/kafkatrace/tracer.go:29`). The field `ClusterIDFetcher kafkaclusterid.Fetcher` is exported, while the old `clusterID`, `clusterIDMu`, and `clusterIDReady` were unexported. The existing `PrevSpan` field is also exported, so this is consistent with the struct's convention. But per the universal checklist on not exporting internal-only fields, consider whether external consumers need direct access to the fetcher. The `ClusterID()`, `SetClusterID()`, `FetchClusterIDAsync()`, `StopClusterIDFetch()`, and `WaitForClusterID()` methods already provide the full API surface.

2. **Parameter ordering in `setProduceCheckpoint`** (`contrib/IBM/sarama/producer.go:234`). The signature changed from `(enabled bool, msg *sarama.ProducerMessage, version)` to `(enabled bool, clusterID string, msg *sarama.ProducerMessage, version)`. Per the contrib-patterns reference on DSM function parameter ordering (cluster > topic > partition), `clusterID` before `msg` makes sense. This is fine.

3. **`setClusterID` is defined but never called** in the IBM/sarama config (`contrib/IBM/sarama/option.go:34-36`). The `setClusterID` method is defined on `config` but no call site in this PR uses it. Per the universal checklist on unused API surface, consider removing it unless it is planned for near-future use.

## Overall assessment

Good refactoring that extracts shared cluster ID logic into `instrumentation/kafkaclusterid` and adds proper cancellation support via context-aware fetching. The `Stop()` replacing `WaitForClusterID()` in `Close()` is a meaningful improvement — it prevents the integration from blocking shutdown on a slow broker. The main concerns are the race condition on Fetcher fields, the duplicated `fetchClusterID` between IBM and Shopify sarama packages, and the error messages lacking impact context.
