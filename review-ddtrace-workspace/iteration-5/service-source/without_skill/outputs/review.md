# PR #4500: feat: collect service override source

## Summary
This PR introduces the `_dd.svc_src` span meta tag to track the origin of service name overrides. It adds `instrumentation.ServiceNameWithSource()` as a unified helper for integrations to set both the service name and its source atomically. Four integrations are covered: gRPC, gin-gonic, go-redis v9, and database/sql. The PR also handles service source inheritance (child spans inherit from parent), service mapping overrides (`opt.mapping`), and ensures no tag is emitted when the service matches the global `DD_SERVICE`.

---

## Blocking

1. **`ServiceOverride` struct in `internal/tracer.go` is used as a tag value, creating a hidden contract between packages**
   - Files: `internal/tracer.go`, `ddtrace/tracer/span.go` (`setTagLocked`)
   - The `ServiceOverride` struct is passed as the value of `tracer.Tag(ext.KeyServiceSource, internal.ServiceOverride{...})`. Inside `setTagLocked`, there is a type assertion `if so, ok := value.(sharedinternal.ServiceOverride); ok { ... }`. If any caller passes a plain string as the value of `ext.KeyServiceSource`, the type assertion silently fails and the tag falls through to normal string/bool/numeric tag handling, which would set `_dd.svc_src` as a regular meta tag with the string value but *not* set `s.service`. This means the service name and service source would be out of sync. This is a fragile contract: nothing in the type system or documentation prevents callers from using `tracer.Tag(ext.KeyServiceSource, "some_source")` directly. Consider:
     - Adding a `setServiceWithSource` method to `Span` to make the contract explicit.
     - Or at minimum, handling the `string` case in `setTagLocked` for `ext.KeyServiceSource` and logging a warning.

---

## Should Fix

1. **`enrichServiceSource` compares against `globalconfig.ServiceName()` at finish time, which can change**
   - File: `ddtrace/tracer/span.go`, `enrichServiceSource` method
   - The method checks `s.service == globalconfig.ServiceName()` to decide whether to suppress the tag. If `globalconfig.ServiceName()` changes between span start and finish (e.g., due to remote config or test setup), the tag may be incorrectly added or suppressed. Consider capturing the global service name at span start time instead of reading it at finish.

2. **`serviceSourceSQLDriver` uses a custom constant `"opt.sql_driver"` but other integrations use `string(instrumentation.PackageX)`**
   - File: `contrib/database/sql/option.go`
   - The database/sql integration uses `"opt.sql_driver"` as the default service source, which follows a different naming pattern (`opt.` prefix) than other integrations that use the package name (e.g., `string(instrumentation.PackageGin)`, `string(instrumentation.PackageGRPC)`). The `opt.` prefix seems reserved for user-explicit overrides like `opt.with_service` and `opt.mapping`. The default driver-derived service name is not really a user override; it is a library default. Consider using `string(instrumentation.PackageDatabaseSQL)` or similar for consistency.

3. **The `registerConfig` now has a `serviceSource` field but it is never set during `Register()`**
   - File: `contrib/database/sql/option.go`, `defaultServiceNameAndSource` function
   - The function checks `if rc.serviceSource != ""` but looking at the diff, `registerConfig.serviceSource` is only populated when `WithService` is used during `Register()`. However, the `Register` function's `WithService` option sets `cfg.serviceName` on the `registerConfig`, but the diff does not show a corresponding `serviceSource` field being set on `registerConfig`. If `registerConfig` does not have its `serviceSource` set when `WithService` is called during `Register()`, the source would incorrectly remain as `serviceSourceSQLDriver` instead of `ServiceSourceWithServiceOption`. Looking at the naming schema test `databaseSQL_PostgresWithRegisterOverride`, the expected source is `ServiceSourceWithServiceOption`, so there must be code setting this. If this is handled elsewhere (e.g., `Register`'s `WithService` sets `serviceSource`), the diff is incomplete; otherwise this is a bug.

4. **`serviceSource` field on `Span` is annotated with `+checklocks:mu` but `inheritedData()` reads it under `RLock`**
   - File: `ddtrace/tracer/span.go`
   - The `inheritedData()` method correctly acquires `s.mu.RLock()` before reading `serviceSource`, which is fine for a read lock. However, `enrichServiceSource()` has the annotation `+checklocks:s.mu` but is called from `finish()` which already holds `s.mu.Lock()`. This is correct but worth verifying that the checklocks analyzer understands this pattern. Not a bug per se, but worth a quick static analysis check.

5. **No test for the case where `SetTag(ext.ServiceName, ...)` is called post-creation**
   - File: `ddtrace/tracer/srv_src_test.go`
   - The PR description mentions `serviceSource` is `set to "m" when SetTag overrides it post-creation`, but there is no test covering the `SetTag(ext.ServiceName, "new-service")` path. If someone calls `span.SetTag("service.name", "foo")` after creation, what happens to `serviceSource`? The `setTagLocked` code for `ext.ServiceName` does not appear to update `serviceSource`, which could leave stale source metadata.

6. **Missing tests for `DD_SERVICE` set scenario with service source**
   - The naming schema test harness runs `ServiceSource` tests with `DD_SERVICE=""`. There are no tests where `DD_SERVICE` is set to a non-empty value to verify that `enrichServiceSource` correctly suppresses the tag when the span's service matches the global service.

---

## Nits

1. **Typo in PR description: "inheritence" should be "inheritance"**

2. **`ServiceNameWithSource` wraps a tag call in a closure -- minor indirection**
   - File: `instrumentation/instrumentation.go`, `ServiceNameWithSource` function
   - The function creates a `StartSpanOption` closure that internally calls `tracer.Tag(...)`. This adds one layer of indirection per span start. For hot-path performance, consider whether this could be simplified, though the overhead is likely negligible.

3. **Comment in `span.go` says `set to "m" when SetTag overrides it post-creation` but "m" is not defined anywhere as a constant**
   - File: `ddtrace/tracer/span.go`, line `serviceSource string ... // tracks the source of service name override; set to "m" when SetTag overrides it post-creation`
   - The value `"m"` appears in tests but is not defined as a named constant. Consider defining it (e.g., `ServiceSourceManual = "m"`) for clarity and consistency.

4. **`harness.RepeatString` helper is introduced but only used for service source assertions**
   - File: `instrumentation/internal/namingschematest/harness/harness.go`
   - This is already used for service name assertions too (visible in existing code), so this is fine. Just noting it for completeness.

5. **gin test asserts `serviceSourceGinMiddleware` as a raw string `"opt.gin_middleware"` in one place**
   - File: `instrumentation/internal/namingschematest/gin_test.go`, line `ServiceOverride: []string{"opt.gin_middleware"}`
   - This hardcodes the string rather than referencing the constant `serviceSourceGinMiddleware`. Since it is in a different package, it cannot reference the unexported constant, but it would be cleaner to export the constant or use a shared one.
