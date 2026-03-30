# Code Review: feat: OTel process context v2 (PR #4456)

**PR:** https://github.com/DataDog/dd-trace-go/pull/4456
**Author:** nsavoire
**Status:** Closed in favour of #4478

This PR updates the OTel process context implementation to v2 per OTEP 4719. It migrates from a msgpack serialization approach to protobuf, moves the mmap logic into its own `internal/otelprocesscontext` package, introduces a `memfd_create` fallback strategy for discoverability, and adds a monotonic timestamp field to the shared-memory header as a readiness signal.

---

## P1 — Must Fix

### [internal/otelprocesscontext/otelcontextmapping_linux.go:902–962] Both-fail path returns error even though a valid mapping was created

When `tryCreateMemfdMapping` fails but the anonymous `mmap` fallback succeeds (lines 916–926), the code continues writing the header and payload to `mappingBytes`. Only at line 955 does it check `memfdErr != nil && prctlErr != nil` and unmap everything. But the anonymous mapping (created at line 921) is already fully populated at that point — it just has no name attached. The net effect is that a valid, readable mapping is unmapped and an error is returned to the caller, so the process context is not published at all, even though the data was correctly written.

The fix should be: if the anonymous mmap succeeded, the mapping is usable regardless of prctl failure. The discoverability guarantee (either memfd or prctl must succeed) is only meaningful if there is no alternative reader path. If readers can find the mapping by address (not by name), this restriction is overly conservative. At minimum, the comment at line 952 ("Either memfd or prctl need to succeed") should be reconciled with the code path that created an unnamed but valid anonymous mapping.

```suggestion
// If memfd succeeded, the mapping is findable via /proc/<pid>/maps by name.
// If only anon mmap succeeded and prctl also failed, the mapping exists
// but is not named — log a warning rather than discarding it.
if memfdErr != nil && prctlErr != nil {
    _ = unix.Munmap(mappingBytes)
    return fmt.Errorf("failed both to create memfd mapping and to set vma anon name: %w, %w", memfdErr, prctlErr)
}
```

(The bug is that control reaches this check only after the anonymous mmap succeeded on the else-branch, so `memfdErr != nil` is always true in that branch — making `prctlErr != nil` the sole deciding factor, which the caller cannot distinguish from a total failure.)

### [internal/otelprocesscontext/otelcontextmapping_linux.go:936–950] Mapping contents written before `Mprotect` but `existingMappingBytes` assigned without mprotect on update path

`createOtelProcessContextMapping` does not call `unix.Mprotect(mappingBytes, unix.PROT_READ)` after writing the content, unlike the old implementation (`internal/otelcontextmapping_linux.go` lines 633–637 in the deleted file). The new implementation omits the mprotect step entirely. This means any goroutine in the process can accidentally overwrite the shared mapping. The old code explicitly made the mapping read-only after writing to enforce the "written once, then read-only" invariant. If this was a deliberate removal, it needs a comment explaining why. If not, it is a regression.

### [internal/otelprocesscontext/otelcontextmapping_linux.go:986–991] Race between zeroing `MonotonicPublishedAtNs` and writing `PayloadSize`

In `updateOtelProcessContextMapping`, line 986 atomically sets `MonotonicPublishedAtNs` to 0 (signaling "in progress"), then line 991 writes `header.PayloadSize` as a plain store. A concurrent reader that loads `MonotonicPublishedAtNs == 0` is supposed to skip the mapping, but the plain write to `PayloadSize` is not ordered with respect to other non-atomic fields on weakly ordered architectures (ARM64). The `memoryBarrier()` call on line 988 helps for subsequent writes, but `PayloadSize` is written *after* the barrier is already past the zeroing point. For full safety, `PayloadSize` should also be written atomically or the barrier placed after all payload writes and before the final timestamp store.

---

## P2 — Should Fix

### [internal/otelprocesscontext/otelprocesscontext.go:58] Silently ignoring `proto.Marshal` error

`PublishProcessContext` discards the error from `proto.Marshal`:

```go
b, _ := proto.Marshal(pc)
```

`proto.Marshal` can return an error for invalid messages (e.g., required fields missing in proto2, or when the message graph contains cycles). Even in proto3, ignoring errors is bad practice. The error should be propagated:

```suggestion
b, err := proto.Marshal(pc)
if err != nil {
    return fmt.Errorf("failed to marshal ProcessContext: %w", err)
}
return CreateOtelProcessContextMapping(b)
```

### [internal/otelprocesscontext/otelcontextmapping_linux.go:870–874] Package-level mutable state is not safe for concurrent use

`existingMappingBytes` and `publisherPID` are package-level variables written and read without any synchronization. If `CreateOtelProcessContextMapping` is called concurrently (e.g., during tracer reconfiguration), there is a data race. The old code had the same issue, but a migration to `sync.Mutex` or `sync/atomic` would prevent panics from concurrent slice header reads. At minimum this should be documented as "not safe for concurrent calls."

### [ddtrace/tracer/tracer_metadata.go:399] `"dd-trace-go"` hardcoded string should use the existing version constant

The `telemetry.sdk.name` attribute is hardcoded as the string `"dd-trace-go"` inside `toProcessContext()`. If this value ever needs to change (or match a constant used elsewhere), this creates a divergence risk. Compare with the deleted code in `otelprocesscontext.go` which also hardcoded it, and `tracer.go` which previously did so too. Consider defining a constant or using whatever constant the rest of the tracer uses for this value.

### [ddtrace/tracer/tracer_metadata.go:409–416] `datadog.process_tags` added even when `ProcessTags` is empty

The `extraAttrs` slice in `toProcessContext()` always appends `"datadog.process_tags"` regardless of whether `m.ProcessTags` is empty, whereas the main `attrs` slice skips attributes with empty values (lines 398–401). This inconsistency means the proto message always contains a `datadog.process_tags` key with an empty string value when process tags are not configured. This wastes bytes and may confuse consumers.

```suggestion
if m.ProcessTags != "" {
    extraAttrs = append(extraAttrs, &otelprocesscontext.KeyValue{
        Key:   "datadog.process_tags",
        Value: &otelprocesscontext.AnyValue{Value: &otelprocesscontext.AnyValue_StringValue{StringValue: m.ProcessTags}},
    })
}
```

### [internal/otelprocesscontext/otelcontextmapping_linux.go:1000–1001] Timestamp collision fix is fragile

The `newPublishedAtNs == oldPublishedAtNs` check with `newPublishedAtNs = oldPublishedAtNs + 1` is a reasonable fallback, but it assumes the clock resolution guarantees distinct values under normal circumstances and that adding 1 to a nanosecond timestamp is meaningful to consumers. A comment explaining the invariant ("consumers detect updates by observing a changed non-zero timestamp") would clarify why this is safe rather than, e.g., using a sequence counter.

### [internal/otelprocesscontext/otelcontextmapping_linux.go:885–900] `tryCreateMemfdMapping` uses `MAP_PRIVATE` for the memfd mapping

The memfd mapping uses `unix.MAP_PRIVATE` (line 899). This means writes to the mapping are copy-on-write private to the process, which is the correct behavior for a publisher-only mapping. However, readers in other processes using `memfd_create` typically access the fd directly via `/proc/<pid>/fd/<fd>` — and since the fd is closed (`defer unix.Close(fd)` at line 895) immediately after mmap, there is no fd for other processes to open. The discoverability is therefore achieved solely through `/proc/<pid>/maps` (the `/memfd:OTEL_CTX` name visible there), and readers must re-open the file from that path. This is correct but subtle; a comment explaining that the fd is intentionally closed and readers use `/proc/<pid>/maps` to find and re-open the memfd would help future maintainers.

### [internal/otelprocesscontext/otelcontextmapping_linux.go:1040–1044] `memoryBarrier()` using `atomic.AddUint64` with zero delta is non-standard

The ARM64 comment says "LDADDAL which will act as a full memory barrier." This is a well-known technique but it is fragile: it depends on the Go compiler and runtime not eliding a zero-delta atomic add, and it is not a documented guarantee. The `sync/atomic` package provides `atomic.LoadUint64`/`StoreUint64` which have defined ordering semantics. Consider replacing the ad-hoc fence with a documented approach or leaving a link to the upstream implementation that uses the same pattern, to make it clear this is intentional.

### [internal/otelprocesscontext/proto/generate.sh] `generate.sh` does not check for `protoc` and `protoc-gen-go` on PATH before running

The script calls `protoc` without first verifying it is installed, giving a confusing error if the tool is absent. A `command -v protoc || { echo "protoc not found"; exit 1; }` guard would improve the developer experience. Also, the script uses `set -eu` but does not set `set -o pipefail`, which means errors in piped commands could be silently swallowed.

### [go.mod:532] New dependency `go.opentelemetry.io/proto/slim/otlp v1.9.0` added only for test use

The `slim/otlp` dependency is added to the root `go.mod` and is only used in `otelprocesscontext_test.go` for the wire compatibility test. This adds an indirect dependency to all consumers of `dd-trace-go`. The PR description notes there is an alternate implementation (#4478) that uses OTLP protos directly, which this PR explicitly avoids to minimize dependencies — yet a test-only OTLP dependency was added anyway. Consider moving the wire compatibility test to a separate `_test` package with a `go:build ignore` tag, or using a test-only `go.mod`.

---

## P3 — Suggestions / Nits

### [ddtrace/tracer/tracer_metadata_test.go:431] Copyright year 2026

`tracer_metadata_test.go` and `otelprocesscontext.go` and `otelprocesscontext_test.go` and `proto/generate.sh` all use copyright year `2026`. The current year at time of authorship appears to be 2025/2026 depending on the commit date. Not critical but inconsistent with other files in the repo that use `2025`.

### [internal/otelprocesscontext/otelcontextmapping_linux_test.go:1089–1112] `getContextFromMapping` in test does not validate permissions or mapping size

The new test version of `getContextFromMapping` removed the permission and size checks that were present in the deleted test (`fields[1] != "r--p"`, `length != uint64(otelContextMappingSize)`). This could cause the test to find an unrelated anonymous mapping that happens to have the same signature bytes, making the test less reliable on systems with many anonymous mappings. The original permission checks were meaningful safety guards.

### [internal/otelprocesscontext/otelcontextmapping_linux.go] `removeOtelProcessContextMapping` is not exported but is called in tests via package-internal access

Since the new tests are in the same package (`package otelprocesscontext`), this is fine. But the function name comment about "it should not be necessary for Go" refers to fork safety — a brief explanation of why Go's runtime makes fork-after-goroutine-start effectively impossible (so the PID check is belt-and-suspenders) would help readers unfamiliar with Go's threading model.

### [internal/otelprocesscontext/proto/processcontext.proto:64] `ProcessContext` comment says "opentelemetry.proto.common.v1.ProcessContext"

The actual upstream proto path for ProcessContext in OTEP 4719 is under `opentelemetry.proto.common.v1`, but the spec is still a PR and the exact package path is not finalized. The comment should note this is provisional and link to the OTEP PR rather than stating a final proto path.

### [ddtrace/tracer/tracer_metadata.go:386] `attrs` slice built with anonymous struct; consider a type alias

The anonymous `struct{ key, val string }` in `toProcessContext()` works fine but a named type like `type kv struct{ key, val string }` at the package level would improve readability and could be reused if the pattern is repeated elsewhere.

---

## Summary

The PR is a well-structured refactor with strong test coverage: it moves mmap logic to a dedicated package, replaces msgpack with protobuf for cross-language compatibility, adds a monotonic timestamp readiness signal, and introduces `memfd_create` as a more reliable discoverability mechanism. The architecture is sound and the wire compatibility test is a nice addition.

The main concerns are: (1) a correctness bug in the both-fail error path that may leave processes without a published context in restricted environments; (2) the removal of the `mprotect` read-only enforcement from the previous version; (3) an unmarshaling race in the update path around `PayloadSize`; and (4) silently ignoring the `proto.Marshal` error. Items 1–3 are potential correctness issues in production environments. The PR is superseded by #4478 but these findings apply to the successor PR as well.
