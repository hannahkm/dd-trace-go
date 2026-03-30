# Review: PR #4250 - franz-go contrib integration

## Summary

This PR adds a new `contrib/twmb/franz-go` integration for tracing the `twmb/franz-go` Kafka client library. It uses franz-go's native hook system (`kgo.WithHooks`) to instrument produce and consume operations, with support for Data Streams Monitoring (DSM). The architecture separates internal tracing logic into `internal/tracing/` to support Orchestrion compatibility (avoiding import cycles).

## Applicable guidance

- style-and-idioms.md (all Go code)
- contrib-patterns.md (new contrib integration)
- concurrency.md (mutexes, shared state across goroutines)
- performance.md (span creation is a hot path)

---

## Blocking

1. **Custom `*Client` wrapper type returned instead of using hooks-only approach** (`kgo.go:9-15`). The contrib-patterns reference explicitly states: "This library natively supports tracing with the `WithHooks` option, so I don't think we need to return this custom `*Client` type (returning custom types is something we tend to avoid as it makes things more complicated, especially with Orchestrion)." The current design returns a `*Client` that embeds `*kgo.Client` and overrides `PollFetches`/`PollRecords`. While hooks are used for produce/consume instrumentation, the `*Client` wrapper exists to manage consume span lifecycle (finishing spans on the next poll). This is a known tension in the design -- but reviewers have strongly pushed back on custom client wrappers when the library supports hooks. Consider whether consume span lifecycle can be managed entirely through hooks (e.g., `OnFetchBatchRead` for batch-level spans rather than per-record spans that need external lifecycle management).

2. **`tracerMu` lock acquired on every consumed record** (`kgo.go:114-120`). `OnFetchRecordUnbuffered` is called for every consumed record and acquires `c.tracerMu` to lazily fetch the consumer group ID. After the first successful fetch, this lock acquisition is pure overhead on every subsequent record. The consumer group ID is write-once after the initial join/sync -- use `atomic.Value` instead (per concurrency.md: "Prefer atomic.Value for write-once fields"), or check once and store with a `sync.Once`. This avoids lock contention on the hot consume path.

3. **`activeSpans` slice grows unboundedly without capacity management** (`kgo.go:127-129`). Each consumed record appends a span pointer to `c.activeSpans`. The slice is "cleared" with `c.activeSpans[:0]` which retains the underlying array. If a consumer polls large batches, this slice will grow to the high watermark and never shrink. More critically, the `activeSpansMu` lock is acquired per-record on append and then again on the next poll to finish all spans. Consider collecting spans at the batch level rather than per-record to reduce lock contention.

4. **Example test exposes `internal/tracing` package to users** (`example_test.go:13,19`). The example imports `github.com/DataDog/dd-trace-go/contrib/twmb/franz-go/v2/internal/tracing` directly, which is an internal package. Users cannot import internal packages. The `WithService`, `WithAnalytics`, `WithDataStreams` options should be re-exported from the top-level `contrib/twmb/franz-go` package, or the example should only use options available from the public API.

## Should fix

1. **Magic string `"offset"` used as span tag key** (`tracing.go:847,912`). The tag key `"offset"` is used as a raw string literal in `StartConsumeSpan` and `FinishProduceSpan`. Per style-and-idioms.md, use named constants from `ddtrace/ext` or define a package-level constant. Check if `ext.MessagingKafkaOffset` or similar exists; if not, define `const tagOffset = "offset"`.

2. **Missing `Measured()` option on produce spans** (`tracing.go:876-891`). Consumer spans include `tracer.Measured()` but producer spans do not. This is inconsistent -- both produce and consume operations are typically metered for APM billing. Other Kafka integrations in the repo (segmentio, Shopify/sarama) include `Measured()` on both span types.

3. **Import grouping inconsistency** (`kgo.go:6-7,10-15`). The imports in `kgo.go` mix Datadog and third-party packages without proper grouping. The blank `_ "github.com/DataDog/dd-trace-go/v2/instrumentation"` import is placed between two Datadog import groups with a comment. Per style-and-idioms.md, imports should be grouped as: (1) stdlib, (2) third-party, (3) Datadog packages.

4. **`SetConsumerGroupID` / `ConsumerGroupID` exported on `Tracer`** (`tracing.go:95-101`). These methods are exported but are only used internally by the `Client` wrapper. Per contrib-patterns.md, functions meant for internal use should not be exported. Make these unexported (`setConsumerGroupID` / `consumerGroupID`).

5. **No comment explaining when hooks are called** (`kgo.go:78,88,98,100`). Per style-and-idioms.md, when implementing interface methods that serve as hooks (like franz-go's `OnProduceRecordBuffered`, `OnFetchRecordUnbuffered`), add a comment explaining when the hook fires and what it does. The existing comments are good but could be slightly more specific about the franz-go lifecycle (e.g., "called by franz-go when a record is accepted into the client's produce buffer, before it is sent to the broker").

6. **`NewKafkaHeadersCarrier` exported from internal package** (`carrier.go:28`). This function is exported and used in test code (`kgo_test.go:1561`). Since it's in `internal/tracing`, it cannot be imported by external users, but it's still cleaner to keep the API surface minimal. Consider whether this needs to be exported or if the test can use the public `ExtractSpanContext` instead.

## Nits

1. **Unnecessary `activeSpans: nil` initialization** (`kgo.go:31`). Zero value of a nil slice is already nil in Go. The explicit `activeSpans: nil` is redundant.

2. **`KafkaConfig` could use a more descriptive name** (`tracing.go:64-66`). The struct only has `ConsumerGroupID`. The comment says "holds information from the Kafka config for span tags" but the name is generic. Consider `ConsumerConfig` or keeping as-is with a note about future expansion.

3. **Test helper `topicName` could use `t.Helper()`** (`kgo_test.go:34`). While it's a simple one-liner, marking it as a helper improves test output readability if it fails.

4. **Inconsistent copyright years** -- Some files say `Copyright 2016`, others say `Copyright 2024`, others say `Copyright 2023-present`. This is minor but worth standardizing for new files.
