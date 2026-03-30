# Review: PR #4468 — feat(datastreams): add manual transaction checkpoint tracking

## Summary

This PR adds a `TrackDataStreamsTransaction` public API and supporting internal machinery to record manual transaction checkpoint observations for Data Streams Monitoring. It introduces a compact binary wire format (matching the Java tracer), a `checkpointRegistry` for name-to-ID mapping, new `Transactions` and `TransactionCheckpointIds` fields on `StatsBucket`, a `ProductMask` bitmask on `StatsPayload`, and regenerated msgpack encoding. The changes are well-scoped and the test coverage is solid.

## Blocking

1. **`api.txt` signature does not match implementation** (`ddtrace/tracer/api.txt`):
   The PR adds `func TrackDataStreamsTransaction(string)` (one `string` parameter) to `api.txt`, but the actual implementation in `data_streams.go` has the signature `func TrackDataStreamsTransaction(transactionID, checkpointName string)` (two `string` parameters). The `api.txt` entry is wrong and will cause API compatibility tooling to report a mismatch. It should be `func TrackDataStreamsTransaction(string, string)`.

2. **`maxTransactionBytesPerBucket` silently drops records with no observability** (`internal/datastreams/processor.go:addTransaction`):
   When the 1 MiB per-bucket cap is exceeded, the transaction is silently dropped with only a `log.Warn`. There is no counter, no metric, no way for the operator to know how many transactions were lost. The existing `stats.dropped` counter is only incremented when `fastQueue.push` fails. For a feature designed for high-throughput pipelines, silent data loss without telemetry is a correctness gap. At minimum, increment a dedicated counter (e.g., `stats.droppedTransactions`) and emit it in `reportStats()` so operators can detect and triage the issue. (The description notes "silently dropped" as intentional for the 254-checkpoint-name limit, which is fine since that's a static configuration issue, but the per-bucket byte cap is a runtime throughput limit where visibility matters.)

## Should fix

3. **Happy path nesting in `addTransaction`** (`internal/datastreams/processor.go:addTransaction`):
   The method has a nested early-return structure that could be flattened. The `if !ok` after `getOrAssign` saves the bucket and returns, then the `if len(b.transactions) >= maxTransactionBytesPerBucket` also saves and returns. The successful path (append + save) is left-aligned, which is good. However, the `getOrAssign` failure branch and the size-limit branch both duplicate `p.tsTypeCurrentBuckets[k] = b` -- consider extracting a deferred save or restructuring so the bucket is always written back once:

   ```go
   // current: two early-return paths both write p.tsTypeCurrentBuckets[k] = b
   // consider: always defer the write-back, or use a single exit path
   ```

4. **`checkpointRegistry.encodedKeys` is shared across all buckets** (`internal/datastreams/processor.go:flushBucket`):
   When a bucket with transactions is flushed, it gets `p.checkpoints.encodedKeys` as its `TransactionCheckpointIds`. This is a reference to the same underlying slice -- if the registry registers new names between when `flushBucket` is called and when the payload is serialized, the `TransactionCheckpointIds` sent on the wire will include checkpoint names that don't correspond to any transaction in that bucket. This is likely benign (the backend should ignore unknown IDs), but it violates the principle of least surprise and could cause subtle debugging confusion. A defensive `slices.Clone(p.checkpoints.encodedKeys)` at flush time would make each payload self-consistent.

5. **Checkpoint name truncation creates collision risk** (`internal/datastreams/processor.go:getOrAssign`):
   Names longer than 255 bytes are truncated to 255 bytes for wire encoding, but the full (untruncated) name is used as the key in `nameToID`. This means two distinct names that share a 255-byte prefix will get different IDs but the wire encoding for both will show the same truncated name. The backend would see two different checkpoint IDs mapping to identical truncated strings. Consider either rejecting names beyond 255 bytes (return `0, false`) or using the truncated name as the map key so they share an ID.

6. **Error message in `flushBucket` uses debug logging but should describe impact** (`internal/datastreams/processor.go:flushBucket`):
   The `log.Warn("datastreams: transaction buffer full, dropping transaction record")` in `addTransaction` tells the operator what happened but not the impact. Per the review convention, it should say something like: `"datastreams: transaction buffer for bucket full (>1 MiB); transaction record for ID %q at checkpoint %q will not appear in DSM transaction monitoring"`.

7. **`TransactionCheckpointIds` field naming** (`internal/datastreams/payload.go`):
   The field uses `Ids` instead of `IDs`, violating Go naming conventions for initialisms. The `//nolint:revive` comment acknowledges this was intentional to match the msgpack wire key. This is acceptable if the wire protocol requires it, but the nolint comment should explain *why* (e.g., `//nolint:revive // wire key must be "TransactionCheckpointIds" to match Java tracer`). The current bare `//nolint:revive` doesn't explain the reasoning.

8. **Test for `sendPipelineStats` with transactions does not verify wire content** (`internal/datastreams/transport_test.go:TestHTTPTransportWithTransactions`):
   The test sends a payload with `Transactions` and `TransactionCheckpointIds` fields but only asserts that one request was made (`assert.Len(t, ft.requests, 1)`). It does not verify that the binary blob survives the msgpack encode -> gzip -> transport round trip. Since this is a new wire format, decoding the request body and verifying the fields would catch serialization regressions.

## Nits

9. **`productAPM` and `productDSM` binary comments are redundant** (`internal/datastreams/payload.go:11-12`):
   The comments `// 00000001` and `// 00000010` next to `uint64 = 1` and `uint64 = 2` are unnecessary -- the decimal values are obvious for single-bit flags. If the intent is to show bit positions, a more conventional Go style would be `1 << 0` and `1 << 1`.

10. **Debug logging in `addTransaction` includes potentially high-cardinality transaction IDs** (`internal/datastreams/processor.go:addTransaction`):
    The `log.Debug("datastreams: addTransaction checkpoint=%q txnID=%q ts=%d", ...)` line logs the full transaction ID. Under high throughput, this could produce enormous log volumes when debug logging is enabled. Consider limiting or omitting the transaction ID from debug logs, or gating it behind a separate verbose flag.

11. **Timestamp deserialization in test uses manual bit shifting** (`internal/datastreams/processor_test.go:TestTransactionBytes`):
    The test manually reconstructs the int64 timestamp with bit shifts. Using `binary.BigEndian.Uint64(b[1:9])` would be cleaner and more obviously correct, matching the encoding path that uses `binary.BigEndian.AppendUint64`.

12. **`noOpTransport` type moved in the test file** (`internal/datastreams/processor_test.go`):
    The `noOpTransport` type and its `RoundTrip` method appear to have been shifted down in the file to accommodate the new test functions. This is fine structurally, but keeping test helpers (like transport mocks) grouped at the bottom of the file is a convention in this codebase.
