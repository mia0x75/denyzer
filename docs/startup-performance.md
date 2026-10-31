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

## List loading

List loading dominates startup on a typical configuration, so it is measured
directly. Every iteration builds a fresh cache and parses the sources from disk,
which is what happens at startup. Times are per operation; lower is better.

Environment: AMD Ryzen 7 5700U (16 threads), Linux amd64, Go 1.26, local
page-cached list files, `-count=5`/`-count=10` medians. The machine was otherwise
idle; numbers on a busy host are higher in absolute terms but scale the same way.

### Denylist and allowlist

One group with 100,000 entries per list (`resolver`, `BenchmarkNewListCaches`):

| Mode | Time |
| --- | --- |
| sequential (before) | 402 ms |
| concurrent (after) | 216 ms |

That is **1.9x** faster, with identical allocations (about 40.6 MB, 1.4M allocs
per operation) — the two caches simply overlap instead of adding up.

### List groups

100,000 entries per group (`lists`, `BenchmarkNewListCacheMultiGroup`):

| Groups | `groupConcurrency: 1` (before) | `auto` (default) | Speedup |
| --- | --- | --- | --- |
| 1 | 278 ms | 288 ms | 1.0x |
| 2 | 549 ms | 290 ms | 1.9x |
| 4 | 1.09 s | 238 ms | 4.6x |

The default `auto` value is `min(number of groups, max(1, loading.concurrency))`,
so the default source concurrency of 4 lets four groups load at once. Memory per
operation is unchanged: the same entries are parsed, only the number of caches
alive at the same moment changes (about 10 MB per 100,000-entry group).

## Reproducing

```bash
go test ./resolver/ -run '^$' -bench BenchmarkNewListCaches -benchtime=5x -count=10
go test ./lists/    -run '^$' -bench BenchmarkNewListCacheMultiGroup -benchtime=3x -count=5
```
