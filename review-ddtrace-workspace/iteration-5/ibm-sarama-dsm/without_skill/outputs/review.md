# PR #4486: feat(dsm): add kafka_cluster_id to IBM/sarama integration

## Summary

This PR adds `kafka_cluster_id` support to both the `IBM/sarama` and `Shopify/sarama` Kafka integrations for Data Streams Monitoring (DSM). It introduces a `WithBrokers` option that auto-fetches the cluster ID via a metadata request, caches it by bootstrap servers, and plumbs it through DSM edge tags, offset tracking, and span tags. The PR also refactors the cluster ID fetching/caching logic from the `confluentinc/confluent-kafka-go/kafkatrace` package into a new shared `instrumentation/kafkaclusterid` package with a `Fetcher` type that provides async fetch with cancellation support.

**Key files changed:**
- `contrib/IBM/sarama/` (consumer, producer, dispatcher, option)
- `contrib/Shopify/sarama/` (option, sarama main file)
- `contrib/confluentinc/confluent-kafka-go/` (kafka.go, kafkatrace/tracer.go for both v1 and v2)
- `instrumentation/kafkaclusterid/` (new shared package: cache.go, fetcher.go, and tests)

---

## Blocking

### 1. Data race in `Fetcher.FetchAsync` -- `cancel` and `ready` fields are not protected

The `Fetcher` struct stores `cancel` and `ready` as plain fields:

```go
func (f *Fetcher) FetchAsync(fetchFn func(ctx context.Context) string) {
    ctx, cancel := context.WithCancel(context.Background())
    f.cancel = cancel
    f.ready = make(chan struct{})
    go func() { ... }()
}
```

If `FetchAsync` is called concurrently, or if `Stop()`/`Wait()` are called while `FetchAsync` is running, there are data races on `f.cancel` and `f.ready`. The `mu` field only protects `f.id`. While the typical usage pattern is sequential (call `FetchAsync` during init, then `Stop` during shutdown), the type's godoc says "It is safe for concurrent use" which is not fully true. Either:
- Protect `cancel` and `ready` with the mutex, or
- Remove the "safe for concurrent use" claim and document the expected usage pattern.

**File:** `instrumentation/kafkaclusterid/fetcher.go`

### 2. Double cache lookup in `fetchClusterID` (IBM/sarama)

The `WithBrokers` option function already checks the cache before calling `FetchAsync`. Then inside `fetchClusterID`, the cache is checked again:

```go
func fetchClusterID(ctx context.Context, saramaConfig *sarama.Config, addrs []string) string {
    key := kafkaclusterid.NormalizeBootstrapServersList(addrs)
    if key == "" { return "" }
    if cached, ok := kafkaclusterid.GetCachedID(key); ok {
        return cached
    }
    // ... network call
}
```

This is harmless but wasteful. More importantly, `NormalizeBootstrapServersList` is called twice (once in `WithBrokers`, once in `fetchClusterID`). The key should be passed as a parameter to avoid redundant computation and ensure consistency. The same issue exists in the `Shopify/sarama` copy.

---

## Should Fix

### 1. `fetchClusterID` only connects to `addrs[0]`

```go
broker := sarama.NewBroker(addrs[0])
```

If the first broker in the list is down, the cluster ID fetch will fail even if other brokers are available. Consider iterating over all provided addresses and returning on the first successful metadata response. The confluent-kafka-go integration uses the admin client which handles this internally, but the sarama integration does not.

**File:** `contrib/IBM/sarama/option.go` (and `contrib/Shopify/sarama/option.go`)

### 2. No timeout on the broker metadata request

The `fetchClusterID` function calls `broker.GetMetadata()` without a timeout. If the broker is reachable but slow to respond, the goroutine launched by `FetchAsync` could hang indefinitely. The context parameter is checked for cancellation before the call, but `GetMetadata` does not accept a context. Consider wrapping the call with a `select` on `ctx.Done()` or setting a deadline on the sarama config's `Net.DialTimeout`/`Net.ReadTimeout`.

**File:** `contrib/IBM/sarama/option.go` (and `contrib/Shopify/sarama/option.go`)

### 3. Identical code duplicated between IBM/sarama and Shopify/sarama

The `WithBrokers`, `fetchClusterID`, `ClusterID()`, and `setClusterID()` implementations are copy-pasted between `contrib/IBM/sarama/option.go` and `contrib/Shopify/sarama/option.go`. While the sarama packages have different import paths (`github.com/IBM/sarama` vs `github.com/Shopify/sarama`), the logic is identical. Consider whether this can be shared via an internal helper that accepts a generic broker interface, or at minimum, document that changes to one must be mirrored in the other.

### 4. `WithBrokers` requires a `*sarama.Config` which users may not have handy

The `WithBrokers` function takes a `*sarama.Config` parameter to pass to `broker.Open()`. This is the same config used by the producer/consumer, but the function signature creates a coupling that makes it awkward if someone wants to use `WithClusterID` (explicitly set) vs auto-detection. This is an API design consideration -- the current API is functional but could be confusing. No change needed if this matches the team's conventions.

### 5. Confluent-kafka-go `WaitForClusterID` is now a no-op wait

The refactored `WaitForClusterID` calls `f.ClusterIDFetcher.Wait()`, which blocks on `f.ready`. But `Close()` now calls `StopClusterIDFetch()` which cancels the context and waits. If user code calls `WaitForClusterID()` and `Stop()` concurrently (from different goroutines), this should work correctly since both read from the same channel. However, `WaitForClusterID` is now documented as "Use this in tests" -- ensure no production code paths depend on it. The rename from blocking-wait to cancel-and-stop semantics on `Close()` is a behavior change worth highlighting in release notes.

---

## Nits

### 1. `MetadataRequest{Version: 4}` is hardcoded

The metadata request version 4 is required to get `ClusterID` in the response. A comment explaining this requirement would help future maintainers understand why version 4 specifically.

### 2. Unused `setClusterID` method

Both `IBM/sarama` and `Shopify/sarama` add a `setClusterID` method to `config`, but it is never called in the diff. If it is intended for future use (e.g., a `WithClusterID` option), consider adding it in the same PR or removing the dead code.

### 3. Test variable shadowing in `TestSyncProducerWithClusterID` (IBM/sarama)

```go
clusterID := fetchClusterID(context.Background(), cfg, kafkaBrokers)
// ...
clusterID, ok := s.Tag(ext.MessagingKafkaClusterID).(string)
```

The `clusterID` variable is reassigned from the fetched cluster ID to the span tag value. While this works because the test asserts they match, it shadows the original value. Using a different variable name (e.g., `spanClusterID`) would improve clarity.

### 4. The `Shopify/sarama` integration is deprecated

The `Shopify/sarama` package was forked and is now maintained as `IBM/sarama`. Adding new features to the deprecated `Shopify/sarama` contrib package may not be necessary if users are expected to migrate. Consider whether this is worth maintaining or if the Shopify version should only receive bug fixes.
