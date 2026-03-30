# Review: PR #4500 - Service source tracking (`_dd.svc_src`)

## Summary

This PR adds service source tracking (`_dd.svc_src`) to spans to identify where a service name override came from. It introduces a `ServiceOverride` struct in `internal/tracer.go` that bundles service name + source to avoid map iteration order nondeterminism (a known P2 issue). The tag is written at span finish time via `enrichServiceSource()`, only when the service differs from the global `DD_SERVICE`. Sources include: `opt.with_service` (explicit `WithService` call), `opt.mapping` (DD_SERVICE_MAPPING), `opt.sql_driver` (SQL driver name), `opt.gin_middleware` (gin's mandatory service name parameter), and package-level defaults (e.g., `"google.golang.org/grpc"`). Changes touch `contrib/database/sql`, `contrib/gin-gonic/gin`, `contrib/google.golang.org/grpc`, `contrib/redis/go-redis.v9`, core span code, and the naming schema test harness.

## Applicable guidance

- style-and-idioms.md (all Go code)
- contrib-patterns.md (multiple contrib integrations touched)
- concurrency.md (span field access under lock)
- performance.md (span creation hot path, setTagLocked)

---

## Blocking

1. **`ServiceOverride` type in `internal/tracer.go` uses exported fields for internal-only plumbing** (`internal/tracer.go:24-27`). The `ServiceOverride` struct with exported fields `Name` and `Source` lives in the top-level `internal` package, which is reachable by external consumers (it's not under an `internal/` subdirectory within a package). This type is used as a value passed through the public `tracer.Tag(ext.KeyServiceSource, ...)` API, meaning users could construct `ServiceOverride` values themselves, creating an undocumented and fragile public API surface. Per the universal checklist: "Don't add unused API surface" and "Don't export internal-only functions." Consider making this an unexported type within `ddtrace/tracer` or moving it to a truly internal package.

2. **`setTagLocked` intercepts `ext.KeyServiceSource` with a type assertion that silently drops non-`ServiceOverride` values** (`span.go:426-434`). If a user calls `span.SetTag("_dd.svc_src", "some-string")`, the `value.(sharedinternal.ServiceOverride)` assertion fails (ok = false), and the function falls through to the normal tag-setting logic, which would set `_dd.svc_src` as a regular string meta tag. This means the meta tag would be set twice -- once by the user's `SetTag` and once by `enrichServiceSource()` at finish. The `enrichServiceSource` write would overwrite the user's value. While this is likely the desired behavior (the system should own `_dd.svc_src`), the silent type assertion swallowing is surprising. Add a comment explaining this behavior, or actively prevent users from setting `_dd.svc_src` directly via `SetTag`.

## Should fix

1. **Ad-hoc service source strings instead of constants** (`option.go:20,32` in database/sql, `option.go:22,203` in gin). The values `"opt.sql_driver"` and `"opt.gin_middleware"` are defined as local package constants but are not centralized. Per style-and-idioms.md and the universal checklist on magic strings: "Use constants from `ddtrace/ext`, `instrumentation`, or define new ones." The `ext.ServiceSourceMapping` and `instrumentation.ServiceSourceWithServiceOption` are properly centralized, but `serviceSourceSQLDriver` and `serviceSourceGinMiddleware` are package-local. If other code needs to reference these values (e.g., in system tests or backend validation), they should be in `ext` or `instrumentation` alongside the other service source constants.

2. **`enrichServiceSource` is called under `s.mu` lock and reads `globalconfig.ServiceName()`** (`span.go:982-994`). `globalconfig.ServiceName()` likely acquires its own lock or reads an atomic. While this is probably safe (no risk of deadlock since `globalconfig` doesn't depend on span locks), calling external functions under a span lock is noted as a pattern to be cautious about in concurrency.md. The value could be cached at span creation or at the tracer level to avoid this.

3. **Missing service source tracking for some contrib integrations**. The PR covers `database/sql`, `gin`, `grpc`, and `go-redis.v9`, but other contrib packages that set service names (e.g., `net/http`, `aws`, `mongo`, `elasticsearch`, segmentio/kafka-go, etc.) are not updated. While it's reasonable to roll this out incrementally, the PR should document which integrations are covered and which remain, or there should be a tracking issue for the remainder. Without this, partial coverage could lead to confusion about which spans do/don't have `_dd.svc_src`.

4. **`startSpanFromContext` in grpc package now takes 5 positional string parameters** (`grpc.go:264-266`). The function signature is `func startSpanFromContext(ctx context.Context, method, operation, serviceName, serviceSource string, opts ...tracer.StartSpanOption)`. Four consecutive string parameters is error-prone -- callers can easily swap `serviceName` and `serviceSource`. Consider using a struct parameter or the option pattern to avoid positional string confusion.

5. **Service source inheritance propagates through child spans even when the child's service matches DD_SERVICE** (`tracer.go:703`). A child span inherits `parentServiceSource` from its parent. If the child's service ends up being the global DD_SERVICE (because no override was applied), `enrichServiceSource` will skip writing the tag (since `s.service == globalconfig.ServiceName()`). This is correct behavior, but the `serviceSource` field still carries the parent's value, which could be confusing for debugging. Consider clearing `serviceSource` in `enrichServiceSource` when the service matches the global service, or adding a comment explaining the inheritance model.

## Nits

1. **Comment in `span_test.go` fixed from incorrect count explanation** (`span_test.go:576`). The comment was corrected from `'+3' is _dd.p.dm + _dd.base_service, _dd.p.tid` to use `+` consistently. Good cleanup.

2. **`harness.RepeatString` helper** (`harness.go`). Nice helper for test readability.

3. **Test `TestServiceSourceDriverName` uses `log.Fatal` instead of `t.Fatal`** (`option_test.go:108,133`). Using `log.Fatal` in a test will call `os.Exit(1)` and skip cleanup. Use `require.NoError(t, err)` or `t.Fatal` instead.

4. **Import grouping in `conn.go`** (`conn.go:14-17`). The new `instrumentation` import is correctly placed in the Datadog group. Good.

The overall design is solid. Using `ServiceOverride` as a compound value passed through `Tag()` to solve the map iteration nondeterminism issue (the P2 finding from concurrency.md) is a clean approach. Writing the tag at finish time via `enrichServiceSource()` avoids polluting the hot tag-setting path.
