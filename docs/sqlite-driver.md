# Decision record: SQLite driver and release pipeline

!!! info "Status"

    **Accepted** — the query log keeps the native SQLite driver and the
    release pipeline is built around it.

    | | |
    |---|---|
    | **Date** | 2026-10-29 |
    | **Decision** | Keep `github.com/mattn/go-sqlite3` (CGO), cross-compile with a Zig `cc` wrapper, build macOS on a macOS runner |
    | **Alternatives** | `modernc.org/sqlite` (pure Go), `ncruces/go-sqlite3` (WASM) |
    | **Supersedes** | The inherited upstream configuration, which builds every target with `CGO_ENABLED=0` |

## Context

Denyzer stores its query log in an embedded SQLite database. The driver is
`github.com/mattn/go-sqlite3`, the native (CGO) driver. The release
configuration, however, was inherited from upstream Blocky and builds every
target with `CGO_ENABLED=0`:

```yaml
# .goreleaser.yml (before)
env:
  - CGO_ENABLED=0
```

That works for Blocky, whose SQLite support came from the pure-Go
`modernc.org/sqlite` chain. For Denyzer it produces a binary that compiles,
starts and even looks healthy — but the SQLite store is a non-functional stub:

```text
Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work.
This is a stub
```

The query log silently falls back to the console writer, the dashboard shows
an empty store and no release binary is actually able to do the job it is
shipped for. The release pipeline therefore had to change one way or the
other, and the only real question was whether to keep the native driver.

Two questions were open:

1. **Which driver should Denyzer use?** The native driver is the fastest,
   but it forces CGO, and CGO costs the whole cross-compilation matrix.
2. **Can the native driver be cross-compiled for the complete release
   matrix?** If not, the release matrix has to shrink or the release has to
   be produced on several platforms.

## Benchmark method

The comparison was run with a throwaway harness that lives **outside** this
repository and is not committed anywhere. It exists only to produce the
numbers below and to validate the cross-compilation wrapper.

### Harness layout

```text
run-all.sh              runs both benchmarks
run-drivers.sh          driver comparison only
drivers/                one Go module per driver
  benchcore/            shared workload, schema, cleanup and reporting
  mattn/                CGO driver, uses the exact production DSN
  modernc/              pure-Go transpiled driver
  ncruces/              pure-Go WASM driver
zigcc/
  cc-zig.sh             GOOS/GOARCH -> Zig triple wrapper
  matrix.sh             cross-compiles the probe for every release target
  probe/                minimal CGO program importing mattn/go-sqlite3
  shim/                 stub libresolv archives for the macOS probe
data/                   resulting database files (recreated per run)
```

The drivers cannot share one binary: `mattn` and `ncruces` both register the
`sqlite3` driver name, so each driver is a separate Go module with its own
`main.go` and its own DSN syntax.

| driver | driver name | busy timeout / WAL / sync syntax |
|---------|-------------|----------------------------------|
| `mattn` | `sqlite3` | `?_busy_timeout=5000&_journal_mode=WAL&...` |
| `modernc` | `sqlite` | `?_pragma=busy_timeout(5000)&_pragma=...` |
| `ncruces` | `sqlite3` | `?_pragma=busy_timeout(5000)&_pragma=...` |

The `mattn` DSN is copied verbatim from `storage/db.go`, so it exercises the
same pragmas that production uses: `journal_mode=WAL`,
`synchronous=NORMAL`, `txlock=immediate`, `cache_size=-65536` and
`mmap_size=256MiB`.

### Workload

The workload is deliberately close to Denyzer's real shape:

1. one transaction with a prepared `INSERT` into `query_logs`,
2. followed by a `COUNT(*)` over a client prefix.

Both are what the asynchronous log writer and the dashboard reporting
queries actually do. Each iteration creates a fresh database, inserts
**500,000 rows** and reads them back.

### Measurement rules

!!! warning "Run this on an otherwise idle machine"

    The numbers vary by a factor of three or more when other services are
    competing for disk and CPU. The harness reports the best *and* the
    median of N iterations so that a noisy run is at least visible, but the
    ordering of the drivers can flip on a loaded host.

The final run was taken on an idle host (load average 1.0 on 16 cores, no
competing workloads, 25 GiB of free memory):

* CPU: AMD Ryzen 7 5700U
* Go: go1.26.10 linux/amd64
* 10 iterations per driver, 500,000 rows per iteration

## Results

### Driver comparison (500k rows, 10 iterations, idle host)

| driver | median rows/s | best rows/s | spread | median read |
|--------|---------------|-------------|--------|-------------|
| mattn (CGO) | 180226 | 205917 | 1.28x | 47 ms |
| ncruces (WASM) | 159452 | 163708 | 1.11x | 191 ms |
| modernc (pure Go) | 59312 | 62496 | 1.14x | 113 ms |

Relative to the median:

| driver | vs. modernc | vs. mattn |
|--------|-------------|-----------|
| mattn (CGO) | 3.04x | 1.00x |
| ncruces (WASM) | 2.69x | 0.88x |
| modernc (pure Go) | 1.00x | 0.33x |

All three drivers wrote the same 500,000 rows, returned the same 244,000
matching rows for the client-prefix count and produced byte-identical
database files (28,610,560 bytes), so the comparison is like for like. The
worst/best spread stays below 1.3x for every driver, so the ranking is
reproducible rather than an artefact of the scheduler.

### What the numbers mean

* **mattn (CGO) is the fastest**, and it is also the fastest on reads
  (47 ms vs. 113 ms / 191 ms for the count query). It stays ahead of the
  pure-Go drivers on exactly the workload Denyzer performs.
* **ncruces is a viable pure-Go replacement** — only about 12 % behind mattn
  on writes — but its read path is roughly 4x slower, which shows up on the
  dashboard's range scans and Top-N queries.
* **modernc is 3x slower than mattn** on this workload. That is the driver
  upstream Blocky uses, and the reason the upstream release pipeline can
  build with `CGO_ENABLED=0`.

For Denyzer's actual traffic profile — a query log written in batches every
30 s, read by a dashboard that a human opens occasionally — the difference
between 180k and 59k rows/s is not a bottleneck. The deciding factor is
therefore not the raw write throughput but the release pipeline.

### Driver sizes (static binaries, `-w -s`)

| build | size |
|-------|------|
| mattn (CGO) | 3.6 MiB |
| modernc (pure Go) | 5.4 MiB |
| ncruces (pure Go) | ~6 MiB |

The native driver produces the *smallest* binary as well, because the
transpiled pure-Go drivers carry a generated SQLite implementation.

## Cross-compilation matrix

The second benchmark cross-compiles a CGO program that imports
`github.com/mattn/go-sqlite3` for every `GOOS`/`GOARCH` pair in Denyzer's
release matrix, using the Zig wrapper:

```text
linux/amd64       OK      windows/amd64     OK
linux/arm         OK      freebsd/amd64     OK
linux/arm64       OK      freebsd/arm       OK
linux/mipsle      OK      freebsd/arm64     OK
linux/mips        OK      netbsd/amd64      OK
linux/mips64le    OK      netbsd/arm        OK
linux/mips64      OK      netbsd/arm64      OK
openbsd/amd64     OK      darwin/amd64      linked*
openbsd/arm       OK      darwin/arm64      linked*
openbsd/arm64     OK

all targets built
```

\* The two macOS rows only *link* in the harness, and they needed two extra
hacks that are not part of the shipped wrapper — see "Why macOS is built on a
macOS runner" below.

Zig ships a libc for the Linux (musl) and Windows (mingw) targets, so those
binaries are self-contained: the Linux ones are statically linked, the
Windows one imports nothing but the Universal CRT that Windows 10 and later
ship. Zig has no libc for the BSDs, so those binaries are dynamically linked
against the system libc (`libc.so.7`), like the binaries of the previous
pure-Go releases.

The wrapper `scripts/zigcc/cc-zig.sh` translates
`GOOS`/`GOARCH`/`GOARM`/`GOMIPS` into the matching Zig target triple and
forwards the invocation to `zig cc`. It rejects macOS targets outright, so a
cross build can never silently produce an unusable macOS binary.

### What the wrapper has to work around

| problem | workaround |
|---------|------------|
| Zig needs an explicit `-target` | `cc-zig.sh` maps `GOOS`/`GOARCH`/`GOARM`/`GOMIPS` to a Zig triple |
| Zig rejects `-fno-PIC` on the mips64 N64 ABI | `-Wno-error=option-ignored` |
| LLD rejects `-Wl,-nopie` and `-Wl,--no-execute-only` on BSD | both flags are filtered out |
| Go probes the linker with `-fuse-ld=bfd` on some targets and requires the output to contain `GNU ld` | the probe is answered with a GNU ld version string and the flag is dropped from the real link |

### Why macOS is built on a macOS runner

The two macOS rows are the reason the macOS cross builds were **not**
accepted as a solution. In the harness they only linked, and only with two
additional hacks: a stub `libresolv` archive had to be linked in, because
Go's darwin runtime always links `-lresolv`, and `-framework CoreFoundation`
had to be dropped, because Zig ships no macOS SDK. The resulting binary was
never run on a real Mac, so the "OK" would have meant nothing more than "the
linker did not complain".

A closer look confirmed that macOS cannot be cross-built from Linux at all.
Go's darwin runtime always links `-lresolv` and imports the `res_9_*`
symbols from `/usr/lib/libresolv.9.dylib`, a file that only exists on a
real macOS host. The macOS binaries are therefore built on a macOS runner
with the native Apple compiler, and the release workflow merges their
archives into the release that the Linux job creates.

## Decision

**Keep `github.com/mattn/go-sqlite3` (CGO).**

1. It is the fastest driver for both the write and the read path.
2. `CGO_ENABLED=0` silently produces a non-functional binary with mattn
   (`Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to
   work. This is a stub`), so the release configuration is broken and has to
   change anyway.
3. The Zig `cc` wrapper cross-compiles the complete release matrix, so CGO
   does not cost the release matrix.

### Consequences

The release pipeline is built around the native driver:

* `.goreleaser.yml` builds with `CGO_ENABLED=1`. The compiler comes from the
  `CC` environment variable, because GoReleaser has no dedicated `cc` field.
* The release configuration contains two build groups that are selected with
  the `DENYZER_TARGET` environment variable:
  * `cross` (the default) — Linux, Windows and the BSDs, built through
    `scripts/zigcc/cc-zig.sh`.
  * `darwin` — macOS on Intel and Apple Silicon, built with the native
    `clang` on a macOS runner.
* `.github/workflows/release.yml` runs the cross build on Ubuntu, the macOS
  build on `macos-latest`, and uploads the macOS archives and the merged
  checksum file to the draft release that the Linux job created. A third job
  takes the binaries of both jobs and runs the smoke test on them.
* The container image is compiled on the target platform, because the
  SQLite driver needs the target's C toolchain. The non-native platforms
  are therefore emulated, which is why the release workflow sets up QEMU.
* `scripts/smoke-test.sh` was added to prove that a release binary really
  talks to SQLite. It starts the server, resolves a name that is answered
  from the custom DNS mapping and reads the entry back through the API. A
  binary built without CGO fails this test, so the release matrix cannot
  silently regress into stub binaries again. The release workflow runs it
  against every binary it builds — Linux on amd64, arm64, armv6 and armv7,
  Windows on amd64 and macOS on amd64 and arm64 — so a broken binary turns
  the workflow red before the draft is published by hand.

### Fallback

If the release matrix ever has to be dropped, or the macOS builds can no
longer be produced on a macOS runner, the fallback is
`github.com/ncruces/go-sqlite3`: it is pure Go, needs no C toolchain, and
costs about 12 % on the write path. Its DSN syntax differs from mattn's
(`?_pragma=...` instead of `?_journal_mode=...`), and the read path is about
4x slower, so the dashboard reporting queries would have to be revisited.

`modernc.org/sqlite` is not a viable fallback for Denyzer: it is 3x slower
than mattn and, like the other pure-Go drivers, it would require rewriting
the DSN and the schema handling in `storage/db.go`.
