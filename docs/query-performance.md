# Query performance optimization plan

## Decision

Denyzer will continue using Go's standard-library `regexp` package.

The regex-engine benchmark in [`regex-engine-benchmark.md`](regex-engine-benchmark.md)
tested the real grouped-cache path with OISD-shaped and literal-prefix rules.
`go-re2` was not a win for this workload and its `re2_cgo` mode conflicts with
the current cross-build environment. `coregex` is much faster for some OISD
scans, but slower for other realistic patterns and has substantially higher
compile and retained-memory costs. Neither result justifies a global engine
replacement.

The optimization target is therefore the work around regex matching:

1. measure the complete query path;
2. evaluate the per-resolver logging plumbing (the lazy variant was rejected, see
   Phase 2);
3. remove avoidable per-query allocations;
4. reduce the number of regex calls with a semantics-preserving blocking
   pre-filter.

YAML work remains outside this plan.

## Current baseline and rules

The existing startup work is complete and documented in
[`startup-performance.md`](startup-performance.md). The query work uses the same
measurement discipline:

- Go `1.27.3`;
- Linux amd64, AMD Ryzen 7 5700U (16 threads), `schedutil` governor;
- the governor stays at `schedutil` — pinning the cores to `performance` lets
  the package hit its power limit and collapses the throughput of the
  CPU-bound cases, exactly as the startup work showed;
- the two sides of a comparison are run alternately (A, B, A, B, ...) and the
  **median** of the rounds is reported;
- seven rounds per side, `-benchtime=1s`;
- no concurrent compile, test, or benchmark process;
- commands run with proxy variables removed:

  ```bash
  env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy \
    -u SOCKS5_PROXY <command>
  ```

`benchstat` cannot be installed in the build environment (no network access),
so the A/B runner is a small local script: it builds both sides once, runs the
alternating rounds, and reports the median, the min-max spread and the delta.
The raw output of every round is kept in the temporary benchmark directory.

Every phase must report `ns/op`, `B/op`, and `allocs/op`, and must include
correctness tests. A speedup that changes blocking decisions, response data,
logging output, or cancellation behavior is not acceptable.

### Noise floor

The host is a shared laptop: the `schedutil` governor moves the core frequency
between roughly 1.1 GHz and 3.6 GHz, so the absolute `ns/op` of a CPU-bound
case depends on the state of the machine at the moment of the run. The only
meaningful comparison is therefore a *relative* one, measured in the same
session.

Running the same build against itself calibrates the method. Seven rounds per
side, both sides identical, `server` package:

| Case | Median `ns/op` | Spread over 14 runs |
| --- | --- | --- |
| `BenchmarkQueryPath/blocked_exact` | 16687 | 15817-17052 |
| `BenchmarkQueryPath/upstream_hit` | 16220 | 15522-18813 |
| `BenchmarkQueryPath/upstream_miss` | 279042 | 254508-330615 |
| `BenchmarkQueryPath/regex_only_miss` | 1879808 | 1477740-3646843 |

Most cases reproduce within ±5 %. The two that do not are the ones whose cost
is dominated by something other than the code under test: `upstream_miss`
includes a real UDP round trip, and `regex_only_miss` runs a linear scan over
1000 compiled regular expressions, which is bound by the memory layout of the
compiled programs.

Two consequences follow for the rest of the plan:

- `allocs/op` and `B/op` are exact and reproducible on every host, so they are
  the primary metric for the allocation work of phase 2 and phase 3;
- the comparison that decides phase 4 has to be a same-binary comparison — the
  reference implementation stays in the test code as a second sub-benchmark, so
  both variants see the same frequency state. This is the pattern the startup
  benchmarks already use.

## Phase 1: establish the complete query baseline

### Objective

Measure the end-to-end query path before changing allocation behavior. The
benchmark must exercise the production composition rather than a synthetic
function call:

```text
server request entry
  -> resolver chain
  -> blocking/cache decisions
  -> mock upstream response
```

### Work

Add a `BenchmarkQueryPath` beside the existing server/resolver benchmarks and
add focused breakdown benchmarks for the expensive sections. Use a local mock
upstream and prebuilt resolver state so network and list-download time do not
hide per-query work.

Measure at least these cases:

| Case | Purpose |
| --- | --- |
| resolver-chain miss to mock upstream | full common miss path |
| cache hit | avoid attributing upstream work to allocation changes |
| blocked exact/wildcard domain | verifies cheap blocking path |
| regex-only miss | isolates the remaining regex scan |
| one-group and multi-group lookup | exposes result-map and group-scan costs |
| debug/trace disabled and enabled | separates normal traffic from logging traffic |

Record the resolver-chain depth and list sizes in the benchmark name. Keep the
existing `BenchmarkCachingResolverResolve` and logging benchmarks as lower-level
references.

### What was added

- `server/query_path_bench_test.go` — `BenchmarkQueryPath` (eight cases),
  `BenchmarkQueryPathLogLevel` (info, debug, trace) and `BenchmarkNewRequest`;
- `resolver/query_path_bench_test.go` — `BenchmarkBlockingResolve` (six cases)
  for the denylist lookup alone;
- `helpertest/benchdata` — the shared list data. The regex-only cases use 1000
  rules sampled with an even stride from the OISD list the repository ships
  (`helpertest/data/oisd-big-wildcard.txt`, 209612 wildcard entries), in the
  shape a real OISD-derived list has: `/^(.*\.)?<domain>$/`. Such a rule has no
  literal prefix, so the engine has to scan it.

The chain under test is the production chain of `createQueryResolver`: 20
resolvers, from the statistics resolver down to the upstream tree. The mock
upstream is an in-process UDP server, so a query that no resolver answers
locally performs a real socket round trip.

Two details of the benchmark setup are worth keeping:

- **The query names use a plain domain** (`query-path-bench.com`). The
  special-use domain names (`test.`, `example.`, `invalid.`, `localhost.`) are
  answered by the special-use domain resolver, which sits *above* the upstream
  tree in the chain. A query for one of them is answered locally and never
  reaches the upstream or the cache — the cache-hit case would silently measure
  the special-use path instead.
- **Every case verifies its own result** before the measurement starts: the
  response type (`RESOLVED`, `CACHED`, `BLOCKED`) and, for the blocked cases,
  the matched rule. A case that stops measuring the path it claims to measure
  fails instead of reporting a wrong number.

### Baseline

`server` package, seven rounds per side, `-benchtime=1s`, median of the 14
runs:

| Case | `ns/op` | `B/op` | `allocs/op` |
| --- | --- | --- | --- |
| `BenchmarkQueryPath/upstream_miss` | 279042 | 15679 | 180 |
| `BenchmarkQueryPath/upstream_hit` | 16220 | 4376 | 53 |
| `BenchmarkQueryPath/blocked_exact` | 16687 | 4214 | 55 |
| `BenchmarkQueryPath/blocked_wildcard` | 18300 | 4278 | 56 |
| `BenchmarkQueryPath/blocked_regex` | 18047 | 4300 | 56 |
| `BenchmarkQueryPath/regex_only_miss` | 1879808 | 5429 | 61 |
| `BenchmarkQueryPath/one_group` | 19903 | 5976 | 70 |
| `BenchmarkQueryPath/multi_group` | 22308 | 6072 | 72 |
| `BenchmarkQueryPathLogLevel/info` | 15249 | 4211 | 55 |
| `BenchmarkQueryPathLogLevel/debug` | 22061 | 6783 | 78 |
| `BenchmarkQueryPathLogLevel/trace` | 23354 | 7231 | 81 |
| `BenchmarkNewRequest` | 5332 | 1531 | 20 |

`resolver` package, same method, measured in a separate session (the absolute
numbers of the two tables are therefore not comparable with each other):

| Case | `ns/op` | `B/op` | `allocs/op` |
| --- | --- | --- | --- |
| `BenchmarkBlockingResolve/exact_hit` | 26690 | 2171 | 32 |
| `BenchmarkBlockingResolve/wildcard_hit` | 28437 | 2235 | 33 |
| `BenchmarkBlockingResolve/regex_hit` | 30958 | 2254 | 33 |
| `BenchmarkBlockingResolve/miss_one_group` | 13032 | 1280 | 14 |
| `BenchmarkBlockingResolve/miss_multi_group` | 18070 | 1376 | 16 |
| `BenchmarkBlockingResolve/regex_only_miss` | 7990986 | 1545 | 14 |
| `BenchmarkCachingResolverResolve` | 8770 | 1688 | 24 |
| `BenchmarkChainLoggingPlumbing` | 87666 | 10152 | 109 |
| `BenchmarkParallelBestResolverSelection/pickRandom/upstreams=2` | 2590 | 0 | 0 |
| `BenchmarkParallelBestResolverSelection/pickRandom/upstreams=5` | 7333 | 0 | 0 |
| `BenchmarkParallelBestResolverSelection/weightedRandom/upstreams=2` | 1568 | 0 | 0 |
| `BenchmarkParallelBestResolverSelection/weightedRandom/upstreams=5` | 3838 | 0 | 0 |

`cache/stringcache` package, same method:

| Case | `ns/op` | `B/op` | `allocs/op` |
| --- | --- | --- | --- |
| `BenchmarkGroupedCacheStringHit` | 496 | 336 | 2 |
| `BenchmarkGroupedCacheMiss` | 15378 | 0 | 0 |
| `BenchmarkGroupedCacheLockContention` | 2184 | 1240 | 4 |
| `BenchmarkGroupedCacheParallel` | 701 | 336 | 2 |
| `BenchmarkLargeList/exact/base` | 98 | 0 | 0 |
| `BenchmarkLargeList/exact/miss-tld` | 69 | 0 | 0 |
| `BenchmarkLargeList/exact/miss-parent` | 62 | 0 | 0 |
| `BenchmarkLargeList/wildcard/base` | 286 | 25 | 1 |
| `BenchmarkLargeList/wildcard/subdomain` | 319 | 25 | 1 |
| `BenchmarkLargeList/wildcard/miss-tld` | 133 | 0 | 0 |
| `BenchmarkLargeList/wildcard/miss-parent` | 99 | 0 | 0 |

### What the baseline shows

- **The logging plumbing is the largest per-query cost.** A chain of 15
  resolvers pays 109 allocations and 10.2 KB per request for the context
  logger alone, before a single line is written. The complete query path
  costs 53 allocations (cache hit) to 180 allocations (upstream miss), so the
  logger rebuild is a large part of the smaller numbers. This is the target of
  phase 2.
- **The denylist lookup itself is cheap.** One group costs 14 allocations and
  1.3 KB; four groups cost 16 allocations and 1.4 KB. The per-group cost is
  small, but the result map that the lookup returns is allocated on every
  lookup — the target of phase 3.
- **The regex scan dominates everything else.** With 1000 OISD-shaped rules the
  regex-only miss needs 1.9 ms in the complete path and 8.0 ms in the isolated
  lookup — more than two orders of magnitude more than the rest of the query
  path, and it is pure CPU time. This is the target of phase 4.
- **The upstream round trip is not the interesting cost.** 180 allocations and
  15.7 KB for a full miss, of which the socket exchange and the response
  packing are a constant part that no allocation change can remove.

### Reproducing

```bash
env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy -u SOCKS5_PROXY \
  go test -run='^$' -bench='BenchmarkQueryPath|BenchmarkNewRequest' -benchmem ./server/

env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy -u SOCKS5_PROXY \
  go test -run='^$' -bench='BenchmarkBlockingResolve|BenchmarkChainLoggingPlumbing' \
  -benchmem ./resolver/

BLOCKY_WILDCARD_LIST=$PWD/helpertest/data/oisd-big-wildcard.txt \
  go test -run='^$' -bench='BenchmarkGroupedCache|BenchmarkLargeList' \
  -benchmem ./cache/stringcache/
```

For a before/after comparison, build both revisions once and run the rounds
alternately:

```bash
go test -c -o /tmp/bench/base.test ./server/     # base revision
go test -c -o /tmp/bench/cand.test ./server/     # candidate revision
# then, for every round, run base and candidate in alternating order and take
# the median of each side
```

### Deliverables and acceptance

- `docs/query-performance.md` updated with the baseline tables and exact
  commands — done;
- raw output retained under the temporary benchmark directory — done;
- no production behavior change — the phase only adds benchmark code and a
  helper package;
- baseline can be reproduced three times on an idle host — the allocation
  figures are exactly reproducible, the `ns/op` figures are host dependent
  within the spread documented above;
- benchmark checks response identity, blocking result, and error behavior —
  every case verifies the response type and the matched rule before it
  measures.


## Phase 2: request logging plumbing

### Problem

Each resolver currently calls `r.log(ctx)`. That rebuilds a context logger and
copies logrus entry state once per resolver even when the configured level will
not emit a log line. The existing
`BenchmarkChainLoggingPlumbing` measures approximately 109 allocations and
10 KB per 15-resolver request-shaped chain.

### Design

- Create the request-scoped log entry once at the server boundary.
- Preserve request fields (`req_id`, `question`, and `client_ip`) and the
  current prefix format.
- Attach the resolver type only at a log site that is actually enabled, or use
  a lightweight resolver-scoped view that does not clone the request context
  for a disabled message.
- Keep expensive values behind level checks. In particular, do not calculate
  `QuestionToString`, `AnswerToString`, obfuscated answers, or formatted error
  text before checking Debug/Trace enablement.
- Keep `uuid.NewV7()` as the request ID generator from the Go 1.27.3 standard
  library migration. Do not replace the externally visible request ID format
  with an unrelated monotonic integer as part of this phase.

### Required tests

- golden tests for prefix and field output;
- tests proving request fields survive every resolver boundary;
- Debug and Trace enabled tests proving the same messages still appear;
- disabled-level tests proving expensive arguments are not constructed;
- race test for concurrent requests sharing the base logger.

### Acceptance target

The Phase 1 baseline gives the numbers to beat:

| Measurement | Baseline |
| --- | --- |
| `BenchmarkChainLoggingPlumbing` (15 resolvers) | 109 allocs, 10152 B |
| `BenchmarkQueryPath/upstream_hit` (complete chain) | 53 allocs, 4376 B |
| `BenchmarkQueryPath/blocked_exact` (complete chain) | 55 allocs, 4214 B |
| `BenchmarkQueryPathLogLevel/info` | 55 allocs, 4211 B |
| `BenchmarkQueryPathLogLevel/trace` | 81 allocs, 7231 B |

The target is a substantial reduction of both numbers — the logging plumbing
must no longer dominate the per-query allocation count, and the trace level
must not cost more than the info level by a multiple. Logging output
compatibility takes priority over a fixed allocation number: the golden tests
have to pass unchanged.

### Result

The lazy design was dropped. It carried only the resolver prefix in the request
context and materialized the prefix through a logrus hook when a line was
emitted. The hook was rejected because it relies on logrus behavior that does
not support it safely:

- `logrus.Logger.Hooks` is a public map. `AddHook`/`ReplaceHooks` write it under
  the logger mutex, but the hook lookup reads it without that lock, so adding a
  hook to a logger that is used concurrently is a data race.
- the prefix was applied when the line was emitted, so a nested resolver
  overwrote the prefix of the outer resolver and the dotted prefix chain
  (`blocking.client_id_cache`) collapsed to its first element.

The final implementation keeps the eager plumbing that was in place before this
phase: every resolver derives its entry from the request entry with
`log.SetPrefix`, which replaces the prefix instead of accumulating it. The
request fields (`req_id`, `question`, `client_ip`) are preserved, the prefix
semantics are unchanged, and the cost is the logrus data-map clone that
`Entry.WithField` performs - one clone per resolver per request. logrus is
scheduled to be replaced by `log/slog`; until then the clone is accepted instead
of adding a second logging mechanism next to logrus.

The golden tests of the `log` package and the resolver tests cover the prefix
and field output; `go test -race ./log ./resolver` covers the concurrent use of
the shared request entry.

Phase 2 therefore contributes no allocation improvement: the plumbing is
identical to the Phase 1 baseline. The numbers below were measured on the same
host with `-benchmem`; the tiny differences in the log-level rows are the
measurement noise of the byte counter.

| Measurement | Phase 1 | Phase 2 |
| --- | ---: | ---: |
| `BenchmarkChainLoggingPlumbing` (15 resolvers) | 109 allocs, 10152 B | 109 allocs, 10152 B |
| `BenchmarkQueryPath/upstream_hit` | 53 allocs, 4376 B | 53 allocs, 4376 B |
| `BenchmarkQueryPath/blocked_exact` | 55 allocs, 4214 B | 55 allocs, 4214 B |
| `BenchmarkQueryPathLogLevel/info` | 55 allocs, 4211 B | 55 allocs, 4209 B |
| `BenchmarkQueryPathLogLevel/trace` | 81 allocs, 7250 B | 81 allocs, 7222 B |

## Phase 3: remove avoidable per-query allocations

Implement and benchmark one change at a time. Do not combine unrelated changes
in a single measurement.

### Resolver race path

Targets in `resolver/parallel_best_resolver.go`:

- make `collectedErrors` lazy: the successful first response should not create
  an error slice;
- replace the small `pickRandom` result slice with a bounded stack-backed
  representation for the current resolver-count limits;
- consider pooling the response channel only after ownership, draining, and
  cancellation rules are explicit. A channel must not return to the pool while
  a losing resolver can still send to it;
- replace `upstreamResolverStatus.lastErrorTime`'s `atomic.Value` containing a
  `time.Time` with an `atomic.Int64` containing a monotonic nanosecond offset,
  if the benchmark confirms the load/store cost matters.

### Request and logging fields

Targets around `server.newRequest`:

- retain UUID v7 semantics, but build the request `logrus.Fields` lazily where
  possible;
- calculate `QuestionToString` only when a Trace message is enabled;
- avoid allocating secondary field maps for a disabled log line.

### Cache result path

Targets in `cache/stringcache/in_memory_grouped_cache.go` and the chained cache:

- measure the single-group result path separately;
- avoid a general-purpose map allocation when the caller needs only one known
  group, while preserving the public `map[string]string` contract at the API
  boundary;
- keep the no-match path allocation-free;
- verify that copy-on-write refreshes remain safe under concurrent lookup.

### Acceptance

- no semantic changes to resolver ordering, cancellation, retry, or error
  joining;
- `go test`, non-e2e Ginkgo suites, and `go test -race` for touched packages;
- allocation reduction demonstrated in Phase 1's complete query benchmark;
- no regression in the existing parallel resolver benchmarks;
- every optimization has a before/after table and a rollback point.

### Result

Phase 3 kept the resolver contracts unchanged while removing avoidable work from
the parallel resolver path:

- error collection is lazy, so a first successful response does not construct an
  error slice;
- upstream error timestamps use an `atomic.Int64` that holds a monotonic
  nanosecond offset (relative to a per-upstream clock base) instead of an
  `atomic.Value` containing a `time.Time`; the wall clock is no longer read, so
  a clock adjustment cannot distort the error window;
- resolver status values are no longer copied when constructing success log
  fields, which avoids copying their atomic state.

A bounded-array rewrite of `pickRandom` was measured and then dropped: it did not
change complete-path allocation counts, and compiler escape analysis confirmed
that the existing selection slice is already stack-backed at the production
call site after inlining. The simpler helper remains in place.

The final candidate was compared with the Phase 1 baseline using seven
alternating A/B rounds, `-benchtime=1s`, and the same `schedutil` host. The
complete-path allocation counts are unchanged: these changes remove work from
paths that were not allocation-dominant in the Phase 1 fixtures, rather than
changing the observable request allocation floor.

| Measurement | Phase 1 baseline | Phase 3 | Phase 3 delta |
| --- | ---: | ---: | ---: |
| `BenchmarkChainLoggingPlumbing` | 109 allocs, 10152 B | 109 allocs, 10152 B | 0 allocs, 0 B |
| `BenchmarkQueryPath/upstream_hit` | 53 allocs, 4376 B | 53 allocs, 4376 B | 0 allocs, 0 B |
| `BenchmarkQueryPath/blocked_exact` | 55 allocs, 4214 B | 55 allocs, 4214 B | 0 allocs, 0 B |
| `BenchmarkQueryPath/upstream_miss` | 180 allocs, 15675 B | 180 allocs, 15675 B | 0 allocs, 0 B |
| `BenchmarkQueryPath/regex_only_miss` | 61 allocs, 5424 B | 61 allocs, 5424 B | 0 allocs, 0 B |

The resolver-selection guardrail remained allocation-free on both sides. Timing
differences are within the host's frequency noise; the exact allocation result
is the important guardrail.

Raw Phase 3 output is retained under `/tmp/opencode/bench/phase3-final2/`.

## Phase 4: blocking regex pre-filter

### Existing behavior

The grouped chain is constructed as regex, wildcard, string, and queried in
reverse order. Therefore exact and wildcard matches already bypass the regex
scan. This behavior is covered by the grouped-cache benchmark and must remain
the first guardrail.

### Implementation

The remaining cost is the regex-only linear scan. The cache now performs a
conservative required-literal analysis once, when a rule is inserted. It uses
`regexp/syntax` only to inspect the pattern; matching continues to use the
standard-library `regexp.Regexp` unchanged. A rule receives a filter literal
only when that literal must occur, byte-for-byte and case-sensitively, in every
possible match. Lookup first uses `strings.Contains` and calls
`Regexp.MatchString` for every candidate that passes the filter.

The analyzer handles concatenation, captures, positive repetitions and
positive bounded repetitions. It deliberately falls back to the normal regex
scan for case-folded literals, alternation, optional or zero repetitions,
character classes, assertions, arbitrary characters, malformed UTF-8 literals,
parse failures, and unknown syntax operations. Thus an unclassified pattern
has exactly the previous behavior; the filter can only skip a pattern that it
has proved cannot match.

The pre-filter must never reject a pattern because of an incomplete parser or
an assumption about regex syntax. Case sensitivity, anchors, escapes,
alternation, Unicode, and empty matches need explicit tests.

### Classification rate

The fallback only costs something when a rule stays unclassified, so the
question is how often that happens. Measured over the 209612 wildcard entries of
`helpertest/data/oisd-big-wildcard.txt`, every rule in the shape a real
OISD-derived regex list has (`/^(.*\.)?<domain>$/`) is classified - the domain
literal is required in every match - so the fallback rate for that list shape is
0%.

The analyzer looks for a literal that must occur in every match, and a literal
in one part of the pattern is enough even when another part is unclassified:

| Pattern | Required literal |
| --- | --- |
| `^(.*\.)?example\.com$` | `example.com` |
| `^ads[0-9]+\.example\.com$` | `.example.com` |
| `^(ads\|analytics)\.example\.com$` | `.example.com` |
| `^example\.(com\|net)$` | `example.` |
| `^ad[sx]\.example\.com$` | `.example.com` |
| `^ads?\.example\.com$` | `.example.com` |
| `.*ads.*` | `ads` |
| `(?i)^ads\.example\.com$` | none (case-folded) |

The rules that stay unclassified are the ones without a usable literal at all:
case-insensitive patterns, patterns whose only literal parts are optional, and
patterns built purely from character classes and assertions. A domain-blocking
rule almost always contains a literal domain fragment, so the fallback rate of
real configurations is low; it is high only for lists that are written as
case-insensitive regexes.

### Measurement matrix

Run the standard-library regex benchmark with and without the pre-filter for:

- OISD-shaped suffix rules;
- literal-prefix rules;
- arbitrary configured patterns;
- 1, 10, 50, 100, 500, and 1000 rules;
- first, middle, last, miss, near-miss, and short-domain queries;
- exact and wildcard hits to prove they still do not enter the regex path.

### Correctness coverage

`cache/stringcache/regex_cache_test.go` now includes:

- required-literal unit cases for anchors, escapes, Unicode, case folding,
  alternation, repetitions, and invalid patterns;
- differential edge-case tests comparing ordered cache results with a plain
  `regexp.Regexp` scan, including mixed classified and fallback rules in both
  insertion orders;
- fuzz seeds for fallback, Unicode, malformed UTF-8, empty-match, repetition,
  and escaped-literal patterns;
- a direct check of every compiled rule in the real OISD-shaped fixture: for
  hit, near-hit, uppercase, suffix, and malformed-UTF-8 query variants, a
  non-empty filter is allowed to skip the rule only when `MatchString` also
  reports no match; and
- zero-allocation miss checks after cache construction.

The grouped-cache guardrails include exact-hit, wildcard-hit, and miss
benchmarks. Exact and wildcard hits still short-circuit before the regex cache,
while a miss still checks every cache and remains allocation-free.

### Result

The matrix was run against the Phase 3 state and the candidate in seven
alternating A/B rounds. The Phase 4 matrix used `-benchtime=100ms` per
sub-benchmark to cover all three pattern shapes, six rule counts, six query
positions, and both implementations; the complete query-path and build
measurements used the same alternating-round method. The table below shows the
1000-rule filtered cases; the standard scan is the `standard` sibling of every
case and remains the semantic reference.

| Pattern shape / query | Standard `ns/op` | Filtered `ns/op` | Delta | `B/op` standard / filtered | `allocs/op` standard / filtered |
| --- | ---: | ---: | ---: | ---: | ---: |
| suffix / first | 1267 | 1275 | +0.6% | 48 / 48 | 1 / 1 |
| suffix / middle | 489701 | 5452 | -98.9% | 48 / 48 | 1 / 1 |
| suffix / last | 1000657 | 9838 | -99.0% | 48 / 48 | 1 / 1 |
| suffix / miss | 1094059 | 13500 | -98.8% | 0 / 0 | 0 / 0 |
| suffix / near-miss | 1225149 | 16115 | -98.7% | 0 / 0 | 0 / 0 |
| prefix / first | 238 | 236 | -0.9% | 32 / 32 | 1 / 1 |
| prefix / middle | 43235 | 6015 | -86.1% | 32 / 32 | 1 / 1 |
| prefix / last | 83893 | 9009 | -89.3% | 32 / 32 | 1 / 1 |
| prefix / miss | 79973 | 15939 | -80.1% | 0 / 0 | 0 / 0 |
| prefix / near-miss | 92050 | 17576 | -80.9% | 0 / 0 | 0 / 0 |
| fallback / first | 1599 | 934 | -41.6% | 32 / 32 | 1 / 1 |
| fallback / middle | 137965 | 143397 | +3.9% | 32 / 32 | 1 / 1 |
| fallback / last | 283428 | 515574 | +81.9% | 32 / 32 | 1 / 1 |
| fallback / miss | 153589 | 214007 | +39.3% | 0 / 0 | 0 / 0 |

The first/near-miss standard values in the compact table can move with CPU
frequency; the raw seven-round reports retain the exact medians and spreads.
The complete matrix also covers 1, 10, 50, 100, and 500 rules plus the `short`
query. Classified suffix and prefix rules show the intended large reduction
when the match is late or absent.

Fallback rules are intentionally not classified, so they pay one extra branch
per rule (`required != ""`) and one wider cache entry (24 bytes instead of 8).
The fallback rows of the table are dominated by the host's timing noise: six
additional rounds of the 1000-rule fallback miss case produced 72.7-338.8 us
for the standard scan and 80.8-189.9 us for the filtered scan, so the two
distributions overlap completely. An isolated alternating A/B of the two cache
representations, with the same 1000 case-insensitive rules and the same query,
measured a median difference of +4.3%, +1.6% and -0.6% over three runs, which
is about 3 ns per rule. The +81.9% of the fallback last-hit row is therefore
not a systematic regression but the spread of the host's `schedutil` governor.

On the complete production query path, the real-list regex-only cases improved
without changing allocation counts:

| Case | Base `ns/op` | Candidate `ns/op` | Delta | `B/op` base / candidate | `allocs/op` base / candidate |
| --- | ---: | ---: | ---: | ---: | ---: |
| `BenchmarkQueryPath/regex_only_miss` | 1919695 | 63298 | -96.7% | 3568 / 3568 | 49 / 49 |
| `BenchmarkQueryPath/regex_late_hit` | 1219708 | 45856 | -96.2% | 3344 / 3421 | 50 / 50 |

Exact and wildcard paths remained separate guardrails. In the same run,
`blocked_exact` was `3285 B / 49 allocs` on both sides and
`blocked_wildcard` was `3348 B / 50 allocs` on both sides. The grouped-cache
short-circuit benchmarks likewise kept exact hits at `336 B / 2 allocs` and
wildcard hits at `360 B / 3 allocs`; their timing differences were within the
host spread. The regex-only improvement therefore does not come from changing
resolver ordering or making cheap cache hits enter the regex path.

The trade-off is paid at list load time. On the full real OISD-shaped regex
fixture (`BenchmarkRegexFactory`, about 209k rules), the seven-round medians
were:

| Measurement | Base | Candidate | Delta |
| --- | ---: | ---: | ---: |
| Build time | 3.00 s | 3.94 s | +31.2% |
| `B/op` | 1.81 GB | 2.16 GB | +19.5% |
| `allocs/op` | 9.65 M | 14.18 M | +46.9% |

The instrumented runs observed peak heap of approximately 440.9 MB for the
base and 449.5 MB for the candidate. The retained/peak-memory increase is much
smaller than the temporary allocation increase, but the startup cost is real
and should be considered for frequent list reloads. The optimization is kept
because query-time regex scans dominate steady-state workloads; the benchmark
and the conservative fallback make the cost visible for future decisions.

Raw Phase 4 output is retained under `/tmp/opencode/bench/phase4/`.

### Acceptance

- differential results equal the standard-library-only implementation for all
  real list fixtures and generated edge cases;
- no false negative is possible for an unclassified pattern;
- regex-only miss and late-hit cases improve in the complete query benchmark;
- compile time and memory are measured and documented as a list-loading trade-off;
- the implementation continues to use the standard `regexp` package.

## Execution order and commits

The Go 1.27.3 and UUID migration is independent of the four performance
phases. The direct UUID usage is submitted first as its own conventional
commit:

```text
build(go): use standard library UUID v7
```

That commit contains only the UUID import/API changes and changes the module
requirement from direct to indirect. `github.com/google/uuid` remains in the
module graph because `github.com/oapi-codegen/runtime` uses it transitively;
it cannot be removed without changing that upstream dependency. This
optimization plan and benchmark report remain documentation changes, not part
of the UUID code commit.

After that migration:

1. Phase 1: baseline benchmark and documentation;
2. Phase 2: request logging plumbing (the lazy variant was evaluated and
   dropped, see the phase result);
3. Phase 3: per-query allocation cuts;
4. Phase 4: blocking regex pre-filter.

Each phase gets its own conventional commit, its own before/after benchmark
table, and its own `gofmt`, build, vet, lint, test, and CodeQL validation. No
alternative regex dependency is added unless a later benchmark disproves the
current decision.

## Risk and rollback checklist

- **Logging compatibility:** compare golden output before and after Phase 2.
- **Cancellation:** run race tests and stress losing parallel upstreams before
  pooling any channels.
- **Regex correctness:** compare every pre-filter decision with standard
  `regexp` results and keep an explicit fallback path.
- **Memory:** measure both Go heap allocations and retained process memory for
  large list loads.
- **Benchmark contamination:** stop or wait for other compiles, rotate A/B
  order, and discard runs made on a busy host.
- **Rollback:** each phase must be revertible without reverting the UUID
  migration or the established startup-performance commits.
