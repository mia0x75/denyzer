# Logging migration and performance

!!! note "Measurement history"
    The tables and validation record source-branch experiments, not checks performed for this port.
    Resolver composition and host/tool availability may differ. Use the reproduction commands to measure the current build.

Denyzer uses Go's `log/slog`. The project `log` package owns its compatible
handlers; there is no Logrus adapter or alternate logger in the request path.
The Logrus comparison revision is `db4735397feb308b3806a15824215c2210f857db`.

## Compatibility

- Configuration retains text/JSON, stdout/stderr/syslog, timestamps, and privacy.
  Level names are case-insensitive: `trace`, `debug`, `info`, `warn`/`warning`,
  `error`, `fatal`, and `panic`. Custom slog levels retain Trace (-8), Fatal (12),
  and Panic (16); JSON still renders Warn as `"warning"`.
- JSON retains `time`, `level`, and `msg`, including time with
  `timestamp: false`. Conflicting user fields move to `fields.time`,
  `fields.level`, and `fields.msg`. Strings use `encoding/json`-compatible
  escaping; arbitrary values retain its marshaling and error semantics.
- Text retains resolver prefixes, dotted sub-prefixes, sorted fields, and nested
  configuration indentation. Syslog receives record severity directly, without
  encoding it into byte markers. Failed syslog setup falls back to stderr.
- Startup buffering snapshots events at capture time and replays them in order
  with the destination's configured level, format, and target. Indentation only
  affects text. Fatal error handling still logs and exits with status 1.
- Existing derived loggers follow output and level reconfiguration. Switching
  waits for in-flight writes before closing an old syslog connection.

`log/logger_compat_test.go`, handler conformance tests using `testing/slogtest`,
Buffer/Recorder tests, syslog tests, resolver field/privacy tests, and subprocess
failure/fatal tests constrain these behaviors.

## Request-path design

Each resolver constructs its prefixed `*slog.Logger` once. The server attaches
`req_id`, `question`, and `client_ip` as context attributes once per request;
`DebugContext`, `InfoContext`, and `LogAttrs` carry them to enabled records without
cloning a logger at every resolver boundary. Explicit initialization/test
loggers still take precedence. Request IDs remain standard-library UUID v7.

Question values snapshot the original question before any resolver mutates it,
then defer formatting and privacy filtering until emission. Answer snapshots
copy mutable DNS records, so their construction is guarded with `Enabled`.
Cold bootstrap hostname refreshes attach the originating upstream's attributes
only on a cache miss, not on every query.

Deferred values and errors resolve once before capture/output fan-out. Recorder
snapshots own groups, maps, slices, and collections nested in arrays, including
cycles; arbitrary mutable pointers and structs are not a deep-copy API. Its
records and message lists are safe to inspect concurrently. Enabled JSON output
renders typed attributes directly rather than constructing a reflection-heavy
`map[string]any` for each event.

No first-party source imports Logrus or its old formatter. Logrus remains an
indirect container-test dependency, not part of the production binary.

## Measurement scope

Do not equate complete query allocations with logging allocations:

| Benchmark | Timed work |
| --- | --- |
| `ResolverLoggingPlumbing/resolver_chain` | 15 reusable resolver lookups and disabled Debug calls; request attributes already attached |
| `ResolverLoggingPlumbing/request_and_chain` | The same chain plus per-request attributes and the original-question snapshot; excludes UUID generation |
| `ChainLoggingPlumbing` | The original 15-resolver control, including backend DNS response construction; request context setup is outside timing |
| `QueryPath` | Production chain (19 resolvers in this branch), blocking/cache decisions, and a real local UDP exchange on a miss; reuses an ingress request |
| `QueryPathWithRequest` | The same path including server request construction on every iteration |
| `QueryPathLogLevel` | Exact blocking at Info/Debug/Trace with real text formatting enabled and writes discarded |
| `NewRequest` | Ingress request construction, including UUID v7 and request context |
| `Logger/text`, `Logger/json` | One enabled event with four bound fields, timestamps, and real formatting; only the write is discarded |

Both sides explicitly configure enabled output. In particular, the baseline
`Silence()` also installs a no-op formatter; changing only its writer would
compare real slog formatting with an empty Logrus formatter.

The baseline worktree has benchmark-only matched Logrus controls for the new
cases; production source is unchanged. These controls do not ship in Denyzer.

## Allocation results

The final confirmation ten-pair comparison measured these median allocations:

| Benchmark | B/op, Logrus → slog | allocs/op, Logrus → slog |
| --- | ---: | ---: |
| `ResolverLoggingPlumbing/resolver_chain` | 9,360 → 0 | 90 → 0 |
| `ResolverLoggingPlumbing/request_and_chain` | 10,192 → 352 | 101 → 5 |
| `ChainLoggingPlumbing` | 10,152 → 792 | 109 → 19 |
| `Logger/text` | 736 → 528 | 18 → 9 |
| `Logger/json` | 1,128 → 512 | 26 → 3 |

The isolated resolver lookups allocate nothing. Including request attributes
and the question snapshot reduces logging bytes by **96.55%** and allocation
count by **95.05%**, exceeding the 90% target in both measures. The complete-chain
control still allocates for its DNS backend; it is not a logging-only result.
Enabled output reduces allocation count by 50.00% for text and 88.46% for JSON.

## Latency and complete-query results

The first final timing cohort had wide confidence intervals on this shared
desktop, reaching 202% in one query case. Its raw output was retained; no
individual samples were removed. The identical prebuilt binaries were measured
again with the same toolchain, CPU, flags, and alternating order. The following
tables use that confirmation cohort; both final cohorts agree on allocation
counts.

Every row has ten samples per revision. The ± percentages are `benchstat`'s 95%
confidence intervals; `~` means no statistically significant latency difference.
No significant latency regression was detected. The reused-request UDP miss
remains noisy and inconclusive, and text output is not claimed to be faster.
These local UDP measurements do not establish latency against external DNS
servers or production tail latency.

| Benchmark | Logrus, µs/op | slog, µs/op | Change | p |
| --- | ---: | ---: | ---: | ---: |
| `ResolverLoggingPlumbing/resolver_chain` | 11.7875 ± 13% | 0.2744 ± 5% | -97.67% | 0.000 |
| `ResolverLoggingPlumbing/request_and_chain` | 13.571 ± 10% | 0.814 ± 17% | -94.00% | 0.000 |
| `ChainLoggingPlumbing` | 14.385 ± 14% | 2.556 ± 12% | -82.23% | 0.000 |
| `Logger/text` | 3.140 ± 21% | 3.147 ± 12% | ~ | 0.315 |
| `Logger/json` | 7.039 ± 12% | 2.914 ± 18% | -58.61% | 0.000 |

Complete-query allocation counts decrease by 36.11–63.93%, rather than the
logging-only reduction above. The ingress-inclusive UDP miss improves by 12.47%
and the ingress-inclusive cache hit by 55.11% in this comparison.

| Benchmark | Logrus, µs/op | slog, µs/op | Change (p) | B/op, Logrus → slog | allocs/op, Logrus → slog |
| --- | ---: | ---: | --- | ---: | ---: |
| `QueryPath/upstream_miss` | 115.1 ± 11% | 105.4 ± 99% | ~ (0.579) | 15,641 → 7,872 | 180 → 109 |
| `QueryPath/upstream_hit` | 10.774 ± 7% | 4.931 ± 30% | -54.23% (0.000) | 4,376 → 976 | 53 → 24 |
| `QueryPath/blocked_exact` | 10.336 ± 4% | 5.910 ± 10% | -42.83% (0.002) | 4,208 → 1,416 | 55 → 31 |
| `QueryPath/blocked_wildcard` | 10.645 ± 10% | 6.239 ± 11% | -41.39% (0.000) | 4,272 → 1,480 | 56 → 32 |
| `QueryPath/blocked_regex` | 10.849 ± 14% | 6.648 ± 7% | -38.73% (0.000) | 4,288 → 1,496 | 56 → 32 |
| `QueryPath/regex_only_miss` | 35.30 ± 8% | 28.07 ± 11% | -20.47% (0.001) | 5,424 → 936 | 61 → 22 |
| `QueryPath/regex_late_hit` | 29.49 ± 9% | 26.20 ± 10% | -11.17% (0.011) | 4,272 → 1,480 | 56 → 32 |
| `QueryPath/one_group` | 13.335 ± 9% | 5.880 ± 5% | -55.90% (0.000) | 5,976 → 1,024 | 70 → 27 |
| `QueryPath/multi_group` | 14.309 ± 16% | 6.852 ± 7% | -52.12% (0.000) | 6,072 → 1,120 | 72 → 29 |
| `QueryPathLogLevel/info` | 9.808 ± 14% | 5.777 ± 9% | -41.10% (0.000) | 4,208 → 1,416 | 55 → 31 |
| `QueryPathLogLevel/debug` | 21.94 ± 11% | 16.11 ± 13% | -26.55% (0.000) | 7,448 → 2,968 | 117 → 62 |
| `QueryPathLogLevel/trace` | 25.64 ± 20% | 19.51 ± 5% | -23.90% (0.000) | 8,152 → 3,448 | 134 → 70 |
| `NewRequest` | 3.623 ± 15% | 1.340 ± 7% | -63.01% (0.000) | 1,530 → 554 | 20 → 10 |
| `QueryPathWithRequest/upstream_miss` | 115.8 ± 22% | 101.4 ± 6% | -12.47% (0.000) | 15,408 → 8,488 | 180 → 115 |
| `QueryPathWithRequest/upstream_hit` | 14.635 ± 23% | 6.570 ± 18% | -55.11% (0.000) | 5,720 → 1,632 | 70 → 33 |
| `QueryPathWithRequest/blocked_exact` | 14.390 ± 31% | 7.446 ± 20% | -48.26% (0.002) | 5,568 → 2,072 | 72 → 40 |

## Running the candidate benchmarks

Use the same Go toolchain on both sides, prebuild binaries, then run them
serially in alternating order. The migration comparison uses Go 1.27.3 with
`GOEXPERIMENT=nodwarf5`, Linux amd64, AMD Ryzen 7 5700U, the existing `schedutil`
governor, CPU 6, `GOMAXPROCS=1`, ten samples per side, and `-benchtime=1s`.
Compare with `benchstat`; only statistically significant latency changes are
claimed.

```bash
mkdir -p /tmp/opencode/slog-bench
go test -c -o /tmp/opencode/slog-bench/resolver.test ./resolver
go test -c -o /tmp/opencode/slog-bench/server.test ./server
go test -c -o /tmp/opencode/slog-bench/log.test ./log

env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY \
  -u http_proxy -u https_proxy -u all_proxy GOMAXPROCS=1 \
  taskset -c 6 /tmp/opencode/slog-bench/resolver.test -test.run='^$' \
  -test.bench='^Benchmark(ResolverLoggingPlumbing|ChainLoggingPlumbing)$' \
  -test.benchmem -test.benchtime=1s -test.count=10 -test.cpu=1

env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY \
  -u http_proxy -u https_proxy -u all_proxy GOMAXPROCS=1 \
  taskset -c 6 /tmp/opencode/slog-bench/server.test -test.run='^$' \
  -test.bench='^Benchmark(QueryPath|QueryPathLogLevel|QueryPathWithRequest|NewRequest)$' \
  -test.benchmem -test.benchtime=1s -test.count=10 -test.cpu=1

env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY \
  -u http_proxy -u https_proxy -u all_proxy GOMAXPROCS=1 \
  taskset -c 6 /tmp/opencode/slog-bench/log.test -test.run='^$' \
  -test.bench='^BenchmarkLogger$' -test.benchmem -test.benchtime=1s \
  -test.count=10 -test.cpu=1
```

The commands above measure one revision. For an A/B result, alternate the
baseline and candidate binaries for each of the ten rounds instead of running
all ten baseline samples before all ten candidate samples. Keep compile, test,
and other benchmark processes out of the measurement window.

## Historical validation

- `make test GINKGO_PROCS='--compilers=1 --no-color'` passed all 19 non-e2e
  suites with coverage enabled.
- Serial `go test -race -count=1 -p=1` passed all 28 non-e2e packages returned
  by `go list ./...`, including packages without tests.
- `golangci-lint run --timeout=5m` reported zero issues.
- `make generate-check` passed after staging regenerated mocks, enums, and the
  configuration schema.
- `make build` with `CGO_ENABLED=1`, `GO_SKIP_GENERATE=1`, and
  `GO_SKIP_DASHBOARD=1` produced the native binary using the existing dashboard
  assets. `scripts/smoke-test.sh` verified a DNS answer and SQLite query-log
  persistence; the frontend was not rebuilt for this check.
- The native Linux/amd64 fully static build and its DNS/SQLite smoke test passed
  after `libc.a` became available. The build used `CGO_ENABLED=1`, the
  `netgo,osusergo,sqlite_omit_load_extension` tags, and
  `-linkmode external -extldflags -static`. `file` confirmed static linking;
  `readelf` showed neither an `INTERP` segment nor `DT_NEEDED` dynamic libraries.
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c ./log` compiled the
  unsupported-syslog platform path. This was compile-only, not a full Windows
  release build or runtime test.
- The JSON string fuzz target passed a 30-second run with 8,712 executions.
- `go mod tidy -diff` and `go mod verify` passed. Production dependency listing
  and first-party source checks found no Logrus import or old formatter.

Focused repeated DoH connection-reuse tests can still fail their exact
connection-count assertions on this host; the failures also reproduced against
the unchanged Logrus baseline. The final serial suites and race checks passed.
No connection-pool behavior or count assertion was changed for this migration.

The initial static-link attempt failed on both revisions while the host lacked
`libc.a`; the successful recheck above uses the host C library, not the
unavailable release cross-build toolchain.

Container and release checks were attempted but could not run in this
environment: Docker/buildx and GoReleaser are absent. Podman is installed, but no
Docker-compatible socket is configured. Zig and `musl-gcc` are also absent, so
the release cross-build matrix was not validated. These limitations are not
passing container or release checks.
