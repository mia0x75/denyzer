# Startup performance

Denyzer prepares several independent things during startup: the denylist and the
allowlist, every group inside each list, the resolvers of the query chain, and the
configured upstreams. These stages used to run strictly one after another. They
now run concurrently wherever they do not depend on each other, which cuts the
time until the first query can be answered.

## What runs concurrently now

| Stage | Before | After |
| --- | --- | --- |
| Denylist and allowlist | loaded one after the other | loaded concurrently |
| List groups | one group rebuilt at a time | up to `loading.groupConcurrency` groups |
| Query resolver components | constructed one after the other | independent components constructed concurrently |
| Upstreams | tested and initialised one by one | upstreams of a group concurrently, groups concurrently |

Only independent work is overlapped: the DNSSEC resolver still waits for the
upstream tree, and client-name resolution still waits for custom DNS and the
hosts file. A single list is never parsed by several goroutines, and the on-disk
download cache (`loading.downloads.cachePath`) remains the way to speed up a
single very large source.

## Method

Every stage is measured with a benchmark that contains two sub-benchmarks:

- `sequential` — the implementation before the change, kept in the test code as
  a reference implementation,
- `concurrent` — the production path after the change.

The two variants are run alternately (A, B, A, B, ...) so that a warm-up of one
variant does not favour the other, and the **median** of the rounds is reported.
Each benchmark round runs the operation `-benchtime` times.

Environment: AMD Ryzen 7 5700U (16 threads), Linux amd64, Go 1.26. The CPU
frequency governor was left at the default `schedutil`. This matters: pinning all
cores to the `performance` governor lets the package hit its power limit, which
collapses multi-core throughput (the list-group benchmark below drops from 2.7x
to 1.8x). The machine was otherwise idle; numbers on a busy host are higher in
absolute terms but scale the same way.

## List loading

List loading dominates startup on a typical configuration, so it is measured
directly. Every iteration builds a fresh cache and parses the sources from disk,
which is what happens at startup. Times are per operation; lower is better.

### Denylist and allowlist

One group with 100,000 entries per list (`resolver`, `BenchmarkNewListCaches`,
`-benchtime=10x`, 3 rounds):

| Mode | Time |
| --- | --- |
| sequential (before) | 1307 ms |
| concurrent (after) | 686 ms |

That is **1.9x** faster, with identical allocations (about 40.6 MB, 1.4M allocs
per operation) — the two caches simply overlap instead of adding up.

### List groups

100,000 entries per group (`lists`, `BenchmarkNewListCacheMultiGroup`,
`-benchtime=3x`, 3 rounds). `groupConcurrency: 1` is the sequential reference,
`auto` is the new default:

| Groups | `groupConcurrency: 1` (before) | `auto` (default) | Speedup |
| --- | --- | --- | --- |
| 1 | 769 ms | 287 ms | ~1.0–2.7x (noisy) |
| 2 | 1641 ms | 967 ms | 1.7x |
| 4 | 2987 ms | 1115 ms | 2.7x |

The default `auto` value is `min(number of groups, max(1, loading.concurrency))`,
so the default source concurrency of 4 lets four groups load at once. Memory per
operation is unchanged: the same entries are parsed, only the number of caches
alive at the same moment changes (about 10 MB per 100,000-entry group).

## Query chain construction

`server`, `BenchmarkNewQueryResolverParts`, `-benchtime=10x`, 3 rounds. The
benchmark builds the complete query chain with a slow list source and a slow
mock upstream (200 ms each), so it reflects the I/O-bound startup work:

| Mode | Time |
| --- | --- |
| sequential (before) | 409 ms |
| concurrent (after) | 208 ms |

That is **2.0x** faster. The concurrent construction runs in two stages: the
independent components (upstream tree, blocking, query logging, conditional
upstream, hosts file, caching resolver) are built at the same time, and the
components that depend on them (client names, DNSSEC) are built afterwards.

## Upstream initialization

`resolver`, `-benchtime=10x`, 3 rounds. Four mock upstreams answer the
initialization test query after 25 ms, so the benchmarks measure how well the
initialization is parallelized:

| Benchmark | sequential (before) | concurrent (after) | Speedup |
| --- | --- | --- | --- |
| `BenchmarkCreateGroupResolvers` (4 upstreams in one group) | 102 ms | 26 ms | 4.0x |
| `BenchmarkCreateUpstreamBranches` (4 groups) | 102 ms | 26 ms | 4.0x |

Both the upstreams inside a group and the groups themselves are initialised
concurrently. The configured order of upstreams and groups is preserved, because
the `strict` strategy depends on it.

## Reproducing

```bash
go test ./resolver/ -run '^$' -bench BenchmarkNewListCaches -benchtime=10x -count=3
go test ./lists/    -run '^$' -bench BenchmarkNewListCacheMultiGroup -benchtime=3x -count=3
go test ./server/   -run '^$' -bench BenchmarkNewQueryResolverParts -benchtime=10x -count=3
go test ./resolver/ -run '^$' -bench 'BenchmarkCreateGroupResolvers|BenchmarkCreateUpstreamBranches' -benchtime=10x -count=3
```

The benchmarks compare the reference implementation with the production
implementation, so the numbers can be reproduced without checking out an older
commit. Run them on an otherwise idle machine and keep the CPU frequency
governor at `schedutil`.
