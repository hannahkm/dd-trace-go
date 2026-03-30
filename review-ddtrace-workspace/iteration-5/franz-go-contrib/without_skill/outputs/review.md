# PR #4250: feat(contrib): add twmb/franz-go integration

## Summary
This PR adds a new Datadog tracing integration for the [twmb/franz-go](https://github.com/twmb/franz-go) Kafka client library. It introduces a `contrib/twmb/franz-go` package that wraps `kgo.Client` with automatic tracing for produce/consume operations, span context propagation through Kafka headers, and Data Streams Monitoring (DSM) support. The internal tracing logic is separated into an `internal/tracing` subpackage to avoid import cycles when supporting orchestrion.

---

## Blocking

1. **Race condition on `tracerMu` lock scope in `OnFetchRecordUnbuffered`**
   - File: `contrib/twmb/franz-go/kgo.go`, `OnFetchRecordUnbuffered` method
   - The `tracerMu` lock is taken to lazily set the consumer group ID, but `c.tracer.StartConsumeSpan(...)` and `c.tracer.SetConsumeDSMCheckpoint(...)` are called *outside* the lock. If `SetConsumerGroupID` is called by one goroutine while another is reading `ConsumerGroupID()` inside `StartConsumeSpan` or `SetConsumeDSMCheckpoint`, there is a data race on `kafkaCfg.ConsumerGroupID`. The lock should encompass all reads of the consumer group ID, or the tracer's `KafkaConfig` should use atomic/synchronized access internally.

2. **`activeSpans` slice grows without bound across the client lifetime**
   - File: `contrib/twmb/franz-go/kgo.go`, `finishAndClearActiveSpans` and `OnFetchRecordUnbuffered`
   - `finishAndClearActiveSpans` resets the length to zero with `c.activeSpans[:0]` but never releases the underlying backing array. If a consumer fetches a large batch (e.g., 100,000 records), the backing array retains that capacity forever. This is a memory leak for long-lived consumers with variable fetch sizes. Consider setting `c.activeSpans = nil` instead of `c.activeSpans[:0]` to allow GC.

---

## Should Fix

1. **Example test imports `internal/tracing` -- leaks internal API to users**
   - File: `contrib/twmb/franz-go/example_test.go`, line with `"github.com/DataDog/dd-trace-go/contrib/twmb/franz-go/v2/internal/tracing"`
   - The `Example_withTracingOptions` function imports `internal/tracing` directly and uses `tracing.WithService(...)`, `tracing.WithAnalytics(...)`, `tracing.WithDataStreams()`. Example tests are rendered in godoc and serve as user documentation. Importing an `internal` package in examples is misleading because users cannot import internal packages. The tracing options should either be re-exported from the public `kgo` package, or the example should use only public API.

2. **`NewClient` does not pass through tracing options**
   - File: `contrib/twmb/franz-go/kgo.go`, `NewClient` function
   - `NewClient` calls `NewClientWithTracing(opts)` without any tracing options. There is no way for users who call `NewClient` to pass tracing options (e.g., `WithService`, `WithDataStreams`). The convenience constructor should either accept variadic tracing options as a second parameter, or the documentation should clearly state that `NewClientWithTracing` must be used for custom tracing configuration.

3. **`OnFetchRecordUnbuffered` ignores the second return from `GroupMetadata()`**
   - File: `contrib/twmb/franz-go/kgo.go`, line `if groupID, _ := c.Client.GroupMetadata(); groupID != "" {`
   - The second return value (generation) is discarded with `_`. While the generation may not be needed for tracing, silently ignoring it means if `GroupMetadata()` ever changes behavior or the generation is needed for DSM offset tracking accuracy, this will be missed. At minimum, add a comment explaining why it is intentionally ignored.

4. **Missing `Measured()` tag on produce spans**
   - File: `contrib/twmb/franz-go/internal/tracing/tracing.go`, `StartProduceSpan` method
   - `StartConsumeSpan` includes `tracer.Measured()` in its span options, but `StartProduceSpan` does not. This is inconsistent with other Kafka integrations (e.g., the Sarama and segmentio/kafka-go contribs) that mark both produce and consume spans as measured. Without this, produce spans may not appear in APM trace metrics.

5. **No span naming integration tests for v1 naming scheme**
   - File: `contrib/twmb/franz-go/kgo_test.go`
   - The test file only checks v0 span names (`kafka.produce`, `kafka.consume`). The `PackageTwmbFranzGo` configuration in `instrumentation/packages.go` defines v1 names (`kafka.send`, `kafka.process`), but there are no tests exercising the v1 naming path. This should be tested to catch regressions.

6. **System-Tests checklist item is unchecked**
   - The PR checklist shows system-tests have not been added. For a new integration, system-tests are important to validate end-to-end behavior across tracer versions and ensure compatibility with the Datadog backend.

---

## Nits

1. **Copyright year inconsistency across files**
   - Some files use `Copyright 2016 Datadog, Inc.` (e.g., `example_test.go`, `carrier.go`, `options.go`) while others use `Copyright 2024 Datadog, Inc.` (e.g., `dsm.go`, `record.go`) and `kgo.go` uses `Copyright 2023-present Datadog, Inc.`. The copyright year should be consistent for newly created files.

2. **`go 1.25.0` in go.mod may be overly restrictive**
   - File: `contrib/twmb/franz-go/go.mod`, line `go 1.25.0`
   - This requires Go 1.25+. Verify this is the intended minimum version for the project. If the repo supports older Go versions, this will prevent users from using the integration.

3. **Magic string `"offset"` used as tag key**
   - File: `contrib/twmb/franz-go/internal/tracing/tracing.go`, lines with `tracer.Tag("offset", r.GetOffset())` and `span.SetTag("offset", offset)`
   - The tag key `"offset"` is a raw string rather than a constant from `ext`. If there is an `ext.MessagingKafkaOffset` constant (or similar), it should be used for consistency. If not, define a local constant.

4. **Blank import comment is test-specific**
   - File: `contrib/twmb/franz-go/kgo.go`, line `_ "github.com/DataDog/dd-trace-go/v2/instrumentation" // Blank import to pass TestIntegrationEnabled test`
   - The comment says this import exists to pass a test. If this import is actually needed for the instrumentation to register itself, the comment should reflect the real purpose rather than citing a test name.

5. **`seedBrokers` variable in tests could be a constant or use an env var**
   - File: `contrib/twmb/franz-go/kgo_test.go`, `var seedBrokers = []string{"localhost:9092", "localhost:9093", "localhost:9094"}`
   - Hardcoding broker addresses makes it difficult to run integration tests in different environments. Consider reading from an environment variable with a fallback default, consistent with other integration test patterns in the repo.
