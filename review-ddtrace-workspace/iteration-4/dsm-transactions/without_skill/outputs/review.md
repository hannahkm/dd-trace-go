# Code Review: PR #4468 -- feat(datastreams): add manual transaction checkpoint tracking

**Repository:** DataDog/dd-trace-go
**PR:** https://github.com/DataDog/dd-trace-go/pull/4468
**Author:** ericfirth
**Status:** MERGED
**Base:** main

## Summary

This PR adds `TrackDataStreamsTransaction` and `TrackDataStreamsTransactionAt` to the public DSM API, allowing users to manually record when a transaction ID passes through named checkpoints in a data pipeline. Transaction records are packed into a compact binary wire format matching the Java tracer protocol and shipped alongside existing stats buckets via the `pipeline_stats` endpoint. Includes a `checkpointRegistry` for stable name-to-ID mapping, `ProductMask` field on `StatsPayload`, per-bucket and per-period size caps, and early-flush behavior when a bucket grows large.

---

## Blocking

### B1. `checkpointRegistry.encodedKeys` slice is shared by reference across concurrent payloads

**File:** `internal/datastreams/processor.go:569-571`

In `flushBucket`, when a bucket contains transactions, the processor sets `mapping = p.checkpoints.encodedKeys`, which is the live backing slice of the registry. This slice reference is then embedded in the `StatsBucket.TransactionCheckpointIds` field and handed to `sendPipelineStats` for serialization. However, the processor's `run` goroutine continues processing new `transactionEntry` items, which call `getOrAssign`, which appends to `r.encodedKeys`. Go's `append` may or may not reallocate, meaning:

- If the slice has spare capacity, `append` mutates the underlying array while `msgp.Encode` reads from it concurrently in `sendToAgent`. This is a data race.
- If the slice is reallocated, the old reference is stale but safe. This happens non-deterministically.

The `sendToAgent` call in `run` (line ~500) serializes the payload in the same goroutine before returning to process more items, so in practice the serialization completes before the next `processInput`. **However**, early-flush paths (`p.earlyFlush` on line 492-500) and the `flushRequest` channel path both call `sendToAgent` synchronously, so this is safe **only** because the run goroutine is single-threaded. This is fragile: any future refactor that moves serialization to a separate goroutine (e.g., async sends) would introduce a data race.

**Recommendation:** Copy the slice before assigning it to the bucket:

```go
mapping = make([]byte, len(p.checkpoints.encodedKeys))
copy(mapping, p.checkpoints.encodedKeys)
```

Alternatively, document the single-goroutine serialization invariant with a prominent comment.

### B2. Per-period rate limiting uses `btime` comparison that breaks with out-of-order timestamps

**File:** `internal/datastreams/processor.go:429-432`

The per-period budget resets when `btime != p.txnPeriodStart`. If `TrackTransactionAt` is called with timestamps from different periods in non-monotonic order (e.g., a batch replaying historical events), the budget resets on every period transition, effectively bypassing the `maxTransactionBytesPerPeriod` limit. For example:

1. Transaction at period A: budget set for A.
2. Transaction at period B: budget resets for B.
3. Transaction at period A again: budget resets for A (now appearing fresh).

Each period switch zeroes `txnBytesThisPeriod`, so across N distinct periods interleaved, you could accept up to `N * maxTransactionBytesPerPeriod` bytes in rapid succession.

**Recommendation:** Either track per-period budgets in a map keyed by `btime`, or document that `TrackTransactionAt` with widely scattered timestamps can exceed the rate limit.

---

## Should Fix

### S1. `TransactionCheckpointIds` sent redundantly with every bucket

**File:** `internal/datastreams/processor.go:569-571`

Every bucket that contains at least one transaction record gets the **full** `encodedKeys` blob (the entire registry mapping). After the first flush, subsequent buckets will repeat all previously registered checkpoint names, not just the ones used in that bucket. This is bandwidth waste that grows linearly with the number of distinct checkpoint names. The Java tracer may do the same, but it is worth confirming. If the backend can handle incremental mappings, only sending new entries since the last flush would be more efficient.

### S2. `checkpointRegistry` name truncation creates silent aliasing risk

**File:** `internal/datastreams/processor.go:256-261`

When a checkpoint name exceeds 255 bytes, the `encodedKeys` blob stores the truncated version, but the `nameToID` map stores the full original string as the key. This means:
- Two names that share the same 255-byte prefix but differ after byte 255 get distinct IDs.
- The `encodedKeys` blob maps both IDs to the same truncated name string.
- The backend cannot distinguish them.

This is an edge case (255-byte checkpoint names are unlikely), but the silent aliasing is surprising. Consider either rejecting names > 255 bytes with a warning, or truncating the key in `nameToID` as well so truly-aliased names share one ID.

### S3. Public API signature diverged from PR diff during iteration

**File:** `ddtrace/tracer/data_streams.go:98`

The PR diff shows the original signature as `TrackDataStreamsTransaction(transactionID, checkpointName string)` (no `context.Context`), but the merged code has `TrackDataStreamsTransaction(ctx context.Context, transactionID, checkpointName string)` and adds span tagging. The `api.txt` entry in the diff still shows `TrackDataStreamsTransaction(string)` with only one string parameter. This `api.txt` appears to have been removed or relocated after the PR was merged, but any downstream tooling that relied on it during the PR's lifetime would have been incorrect.

### S4. No metric or log for per-period transaction drops in `addTransaction`

**File:** `internal/datastreams/processor.go:436-439`

When `txnBytesThisPeriod + recordSize > maxTransactionBytesPerPeriod`, the transaction is silently dropped with only an atomic counter increment. The counter is reported in `reportStats` (line 558-560), but only when the stat is non-zero. Unlike the bucket-size check (which used `log.Warn` in the original diff), this path has no immediate debug/warn log. At high throughput, users investigating missing transactions would have no log-level signal. Consider adding a rate-limited `log.Warn` here, consistent with the registry-full path.

### S5. `earlyFlush` flag could flush stale buckets unnecessarily

**File:** `internal/datastreams/processor.go:455-458` and `processor.go:492-500`

When `earlyFlush` is set, the `run` goroutine calls `p.flush(p.time().Add(bucketDuration))`. This flushes **all** buckets older than `now`, not just the transaction-heavy one. If there are many service-keyed buckets with small stats payloads, they get flushed prematurely. The comment says this matches Java tracer behavior, which is fine, but it means the early-flush transaction path has an amplification effect on non-transaction data.

### S6. `processorInput` struct size increased for all input types

**File:** `internal/datastreams/processor.go:172-178`

Every `processorInput` now carries a `transactionEntry` (two strings + an int64), even for `pointTypeStats` and `pointTypeKafkaOffset` inputs. Since the `fastQueue` holds 10,000 `atomic.Pointer[processorInput]` slots, this does not directly increase the queue's memory footprint (they are pointer-indirected), but each allocated `processorInput` is larger. For high-throughput stats-only workloads, this adds ~40+ bytes per input allocation. Consider using an interface or union-style approach if memory pressure becomes a concern.

---

## Nits

### N1. Debug log format uses `%q` inconsistently

**File:** `internal/datastreams/processor.go:425`

```go
log.Debug("datastreams: addTransaction checkpoint=%q txnID=%q ts=%d", ...)
```

Other debug logs in the same file (e.g., line 454, 570) use `%d` for numeric values but do not quote string values. The `%q` quoting is fine for debugging but is inconsistent with the rest of the file's logging style.

### N2. `//nolint:revive` on `TransactionCheckpointIds`

**File:** `internal/datastreams/payload.go:75`

The `//nolint:revive` directive suppresses the `Ids` vs `IDs` naming lint. The comment explains this matches the backend wire format. This is fine, but the generated `payload_msgp.go` uses the field name as-is for msgpack keys. If the wire format ever changes to `TransactionCheckpointIDs`, this suppression should be removed.

### N3. Test `TestTransactionBytes` manually decodes big-endian int64

**File:** `internal/datastreams/processor_test.go:544-545`

The test manually reconstructs the int64 from individual bytes with bit shifts. Consider using `binary.BigEndian.Uint64(b[1:9])` for clarity and consistency with how the encoding side uses `binary.BigEndian.AppendUint64`.

### N4. Magic numbers in test assertions

**File:** `internal/datastreams/processor_test.go:449`

```go
assert.Equal(t, 42, len(found.Transactions))
```

The value 42 is derived from `3 * (1 + 8 + 1 + 4)`, which is explained in the comment above. Consider using a named constant or computed expression in the assertion for self-documenting tests:

```go
const recordSize = 1 + 8 + 1 + 4 // checkpointId + timestamp + idLen + len("tx-N")
assert.Equal(t, 3*recordSize, len(found.Transactions))
```

### N5. Comment on `productAPM`/`productDSM` says "matching the Java tracer" without a reference

**File:** `internal/datastreams/payload.go:10`

The comment says these match the Java tracer, but provides no file/class reference. Adding a pointer (e.g., `DefaultDataStreamsMonitoring.java`) would help future maintainers verify compatibility.

### N6. `transport.go` line 79 drains `req.Body` instead of `resp.Body`

**File:** `internal/datastreams/transport.go:79`

```go
defer io.Copy(io.Discard, req.Body)
```

This drains the **request** body, not the response body. The response body is already closed by `defer resp.Body.Close()` on line 78, but for correctness the discard should target `resp.Body` to ensure the response is fully consumed before the connection is returned to the pool. The `resp.Body.Close()` on the line above may or may not drain the body depending on the HTTP implementation. This is a pre-existing issue, not introduced by this PR, but it is in a function modified by the PR.

---

## Overall Assessment

The core design is sound: compact binary wire format, checkpoint registry with bounded IDs, size caps, and early-flush behavior. The code is well-documented and the test coverage is thorough, covering edge cases like registry overflow, long IDs, long names, high volume, and the public API delegation path.

The primary concern is **B1** (the shared slice reference for `encodedKeys`), which is currently safe due to the single-goroutine processing model but is fragile. **B2** (per-period budget bypass with out-of-order timestamps) is a real semantic issue for the `TrackTransactionAt` variant. The "should fix" items are mostly about efficiency and observability improvements.
