# Code Review: PR #4456 — feat: OTel process context v2

**PR:** https://github.com/DataDog/dd-trace-go/pull/4456
**Author:** nsavoire (Nicolas Savoire)
**Status:** Closed in favour of #4478

---

## Summary

This PR updates the OTel process context implementation from v1 (msgpack serialization over anonymous mmap) to v2 (protobuf serialization, aligned with OTEP 4719). Key changes:

- Replaces the msgpack-based `otelProcessContext` struct with standalone protobuf-generated types that are wire-compatible with OTLP protos but avoid import conflicts.
- Moves the mmap implementation from `internal/` to a new `internal/otelprocesscontext/` package.
- Upgrades the shared memory mechanism to support both `memfd_create` (preferred, discoverable by fd path) and anonymous mmap + prctl (fallback).
- Adds a `MonotonicPublishedAtNs` field to the header for lock-free reader synchronization.
- Changes the header version from 1 to 2.

---

## Findings

### Critical / Bugs

#### 1. Data race on `existingMappingBytes` and `publisherPID` — no mutex

`existingMappingBytes` and `publisherPID` are package-level variables read and written from `CreateOtelProcessContextMapping` without any synchronization. `storeConfig` (in `tracer.go`) is documented as being called from multiple paths, and nothing prevents two goroutines from racing here. The old code had the same issue, but this PR adds an `updateOtelProcessContextMapping` path that reads `existingMappingBytes[0]` directly, which makes the existing race worse, not better.

Recommendation: protect with a `sync.Mutex`, or at minimum document the assumption that this function is only ever called sequentially.

#### 2. `memoryBarrier()` is incorrect — the atomic add to a local variable is not a global barrier

```go
func memoryBarrier() {
    var fence uint64
    atomic.AddUint64(&fence, 0)
}
```

A `sync/atomic` operation on a **local stack variable** that is never read again provides no ordering guarantee for other memory accesses on architectures that don't elide the operation. On amd64 the `LOCK XADD` implied by the atomic is a full memory barrier, but the Go memory model does not promise this. On ARM64 the comment claims `LDADDAL` will be emitted, but that is only true if the compiler can't prove the address is not aliased — a stack-local variable is a prime candidate for optimization. The stated intent (ensuring writes to `existingMappingBytes` are visible before the `MonotonicPublishedAtNs` store) can only be reliably achieved by using `atomic.StoreUint64` for every field that must be ordered, or by restructuring the update to be a single atomic pointer swap. As written the barrier may be silently removed by the compiler.

#### 3. `proto.Marshal` error is silently ignored in `PublishProcessContext`

```go
func PublishProcessContext(pc *ProcessContext) error {
    b, _ := proto.Marshal(pc)
    return CreateOtelProcessContextMapping(b)
}
```

`proto.Marshal` can return a non-nil error (e.g., if the message contains types that fail to encode). Discarding it means the caller gets no signal that serialization failed and an empty or partial payload may be published. The error should be returned.

#### 4. `updateOtelProcessContextMapping` does not call `mprotect` — no read-only protection after update

`createOtelProcessContextMapping` sets the mapping to `PROT_READ` after writing. `updateOtelProcessContextMapping` does not. It writes directly into `existingMappingBytes`, which was previously made read-only (via `Mprotect`). This will cause a `SIGSEGV` at runtime when the update path is exercised on the second call to `CreateOtelProcessContextMapping`. The old code called `Mprotect` in `createOtelProcessContextMapping` only; the new code adds an update path but forgets to re-protect.

Wait — re-reading the new `createOtelProcessContextMapping` more carefully: the new code does **not** call `unix.Mprotect(mappingBytes, unix.PROT_READ)` at all (the old code did). So the protection is gone entirely. This is a regression versus the v1 implementation, and it means the mapping is writable after creation, undermining the intended read-only guarantees.

#### 5. `getContextFromMapping` in test dereferences a virtual address from `/proc/self/maps` as a raw pointer

```go
header := (*processContextHeader)(unsafe.Pointer(uintptr(vaddr)))
```

This pattern is used in the test to verify reading back the published data. It works only because the test is running in the same process. However, it is essentially identical to a UAF-class dereference if `vaddr` belongs to a freed mapping. It also only works as a test for the happy path. The real-world reader (a profiler agent) will be in a different process and will need to open `/proc/<pid>/mem`. The test doesn't exercise that path at all. This is an observation about test fidelity rather than a production bug, but it means the test doesn't validate the cross-process semantics that this feature exists to provide.

---

### Design / Architecture Concerns

#### 6. `extraAttributes` is not wire-compatible with any established OTLP message

The `.proto` file defines `ProcessContext` with `extra_attributes` at field number 2. The comment says this is wire-compatible with `opentelemetry.proto.common.v1.ProcessContext`, but the upstream OTEP 4719 schema has not been finalized. If the upstream definition changes field numbers, this will silently produce incorrect data. The PR acknowledges this is a draft spec, but there is no mechanism (e.g., a comment, a test against a pinned upstream schema) to flag when the upstream changes.

#### 7. The `datadog.process_tags` extra attribute is always included even when empty

In `toProcessContext()`, the standard attributes skip empty values:
```go
if a.val == "" {
    continue
}
```
But `extraAttrs` (including `datadog.process_tags`) is always appended unconditionally. When `m.ProcessTags` is empty, a `KeyValue` with an empty string value is still published. This is inconsistent with the handling of other attributes and may produce noise in consumers.

#### 8. No `Mprotect` on the mapping after write — regression from v1

As noted in finding #4, the v1 code called `unix.Mprotect(mappingBytes, unix.PROT_READ)` after writing. The v2 code does not. This is a security-relevant regression: any accidental write to the mapping region (e.g., a buffer overflow) would silently corrupt what agents read instead of crashing visibly.

#### 9. `roundUpToPageSize` is called every time but `os.Getpagesize()` allocates a syscall each call

`os.Getpagesize()` is not cached inside `roundUpToPageSize`, and it is called twice per `createOtelProcessContextMapping` invocation (once inside `roundUpToPageSize` and once via `minOtelContextMappingSize = 2 * os.Getpagesize()`). This is minor, but `os.Getpagesize()` is documented to return a constant — caching it once at init time (or using a `var` initialized at package init) would be cleaner.

#### 10. `memfdErr` vs `prctlErr` logic could result in a mapping that is not discoverable

The logic is:
```go
if memfdErr != nil && prctlErr != nil {
    _ = unix.Munmap(mappingBytes)
    return fmt.Errorf(...)
}
```

If only one of the two mechanisms succeeds, the mapping is left and returned successfully. But for a reader using `/proc/<pid>/maps`, a `memfd`-based mapping will appear as `/memfd:OTEL_CTX (deleted)` (because `fd` was closed after `mmap`), while an anonymous mapping named via prctl appears as `[anon:OTEL_CTX]`. The `isOtelContextName` function in the test handles both, but this dual-mode behaviour adds complexity and the comment "Either memfd or prctl need to succeed" is the only documentation. It would help to clarify in comments what each discovery mechanism is and which agent versions support each.

---

### Code Quality / Nits

#### 11. `restoreOtelProcessContextMapping` helper name is misleading

The function is named `restoreOtelProcessContextMapping` but it only registers a cleanup — it doesn't restore anything. `cleanupOtelProcessContextMapping` or `registerMappingCleanup` would be more accurate.

#### 12. Commented-out function name in test helper

```go
// restoreMemfd returns a cleanup function that restores tryCreateMemfdMapping.
func mockMemfdWithFailure(t *testing.T) {
```

The comment says "returns a cleanup function" but the function is `void` — the cleanup is registered via `t.Cleanup`. The comment is stale copy-paste and should be removed or corrected.

#### 13. `go.mod` adds `go.opentelemetry.io/proto/slim/otlp v1.9.0` for test-only use

The slim OTLP proto dependency is used only in `otelprocesscontext_test.go` for wire-compatibility verification. Adding a module dependency for a test-only import increases binary size and dependency surface for all consumers of this module. Consider using a `_test` build tag isolation or a separate sub-module for this dependency.

#### 14. `toProcessContext` leaks Datadog-internal `datadog.process_tags` field name into the shared OTel mapping

The `datadog.process_tags` key in `extraAttributes` is a Datadog-proprietary extension placed in a mapping that is intended to be consumed by OTel-compatible tools. This is a semantic concern: any consumer that doesn't know about this key will silently ignore it, but it couples the inter-process format to an internal Datadog concept. A comment explaining the rationale would help reviewers evaluate this decision.

#### 15. Test file copyright says 2026 but code file says 2025

`otelcontextmapping_linux.go` has `Copyright 2025`, while `otelprocesscontext.go`, `processcontext.pb.go`, and `tracer_metadata_test.go` have `Copyright 2026`. The inconsistency is minor but worth normalizing.

---

## Summary Table

| # | Severity | Category | File |
|---|----------|----------|------|
| 1 | High | Data race | `otelcontextmapping_linux.go` |
| 2 | High | Correctness (memory model) | `otelcontextmapping_linux.go` |
| 3 | High | Silent error discard | `otelprocesscontext.go` |
| 4 | High | Missing mprotect / regression | `otelcontextmapping_linux.go` |
| 5 | Medium | Test fidelity (no cross-process test) | `otelcontextmapping_linux_test.go` |
| 6 | Medium | Design (proto spec stability) | `proto/processcontext.proto` |
| 7 | Low | Inconsistent empty-value handling | `tracer_metadata.go` |
| 8 | Low | Dependency scope | `go.mod` |
| 9 | Low | Code quality | `otelcontextmapping_linux.go` |
| 10 | Low | Nit | test helpers |
| 11 | Low | Nit | test comment |
| 12 | Info | Semantic concern | `tracer_metadata.go` |
| 13 | Info | Copyright inconsistency | multiple files |

---

## Overall Assessment

The design direction is sound: moving to protobuf makes the format more self-describing and easier for heterogeneous consumers to decode, and adding a `memfd_create` path improves discoverability. However, there are three high-severity issues (data race on globals, unreliable memory barrier, silently dropped serialization error) and a regression (no `mprotect` on write-complete) that should be addressed before merging. The wire-compatibility test is a nice addition. The `memoryBarrier` implementation in particular needs to be replaced with a proper atomic store pattern or a `sync.Mutex`-guarded write.
