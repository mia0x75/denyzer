[![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/mia0x75/denyzer/makefile.yml "Make")](https://github.com/mia0x75/denyzer/actions/workflows/makefile.yml)
[![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/mia0x75/denyzer/release.yml "Release")](https://github.com/mia0x75/denyzer/actions/workflows/release.yml)
[![GitHub latest version](https://img.shields.io/github/v/release/mia0x75/denyzer "Latest version")](https://github.com/mia0x75/denyzer/releases)
[![GitHub Release Date](https://img.shields.io/github/release-date/mia0x75/denyzer "Latest release date")](https://github.com/mia0x75/denyzer/releases)
[![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/mia0x75/denyzer "Go version")](#)

# Denyzer

Denyzer is a DNS proxy and ad-blocker for the local network written in Go.

## Screenshots

The dashboard is served by the Denyzer binary itself at `/dashboard/` - no
separate web server and no external database.

### Overview

![Dashboard overview with the all-time counters, the blocking status and the queries-over-time chart](https://raw.githubusercontent.com/mia0x75/denyzer/main/screenshots/dashboard-overview.png)

All-time counters (total queries, blocked ratio, cache hit rate, listed
domains), the blocking status with its pause presets, the maintenance actions
and the query volume over time - switchable between 1h, 24h, 7d and 30d and
filterable per client.

### Top lists

![Top lists with the most active domains and clients](https://raw.githubusercontent.com/mia0x75/denyzer/main/screenshots/dashboard-top-lists.png)

The most active domains and clients, ranked independently for *All* and
*Blocked* queries. The window (1h, 24h, 7d, 30d) and the number of rows (5, 10,
25, 50) are selected independently; the 7-day and 30-day rankings are served
from the trailing-window rollups, so they stay exact no matter how much traffic
the proxy handles.

### Query logs

![Query logs table with filters and pagination](https://raw.githubusercontent.com/mia0x75/denyzer/main/screenshots/dashboard-query-logs.png)

Every logged query with its time, client, domain, record type, response reason
and duration. Filter by domain or client, by response type and by DNS record
type; pagination uses a keyset cursor, so deep pages stay as fast as the first
one. Auto-refresh (every 30s) can be switched on for a live view.

## Features

- **Blocking** - Blocking of DNS queries with external lists (Ad-block, malware) and allowlisting

  - Definition of allow/denylists per client group (Kids, Smart home devices, etc.)
  - Periodical reload of external allow/denylists
  - Regex support
  - Blocking of request domain, response CNAME (deep CNAME inspection) and response IP addresses (against IP lists)

- **Advanced DNS configuration** - not just an ad-blocker

  - Custom DNS resolution for certain domain names
  - Conditional forwarding to external DNS server
  - Upstream resolvers can be defined per client group

- **Performance** - Improves speed and performance in your network

  - Customizable caching of DNS answers for queries -> improves DNS resolution speed and reduces amount of external DNS
    queries
  - Using multiple external resolver simultaneously
  - Low memory footprint

- **Various Protocols** - Supports modern DNS protocols

  - DNS over UDP and TCP
  - DNS over HTTPS (aka DoH)
  - DNS over TLS (aka DoT)
  - DNS over QUIC (aka DoQ, RFC 9250)
  - DNS over HTTPS/3 (aka DoH3, RFC 9114)

- **Security and Privacy** - Secure communication

  - Supports modern DNS extensions: DNSSEC, eDNS, ...
  - DNSSEC validation of upstream resolvers
  - Free configurable blocking lists - no hidden filtering etc.
  - Provides DoH Endpoint
  - Uses random upstream resolvers from the configuration - increases your privacy through the distribution of your DNS
    traffic over multiple provider
  - Denyzer does **NOT** collect any user data, telemetry, statistics etc.

- **Integration** - various integration

  - Embedded web dashboard (DenyzerUI) served at `/dashboard/`
  - Query logging to an embedded [SQLite](https://sqlite.org) store (per-day / per-client), with
    configurable retention - no external database required
  - Various REST API endpoints
  - CLI tool

- **Simple configuration** - single or multiple configuration files in YAML format

  - Simple to maintain
  - Simple to backup

- **Simple installation/configuration** - denyzer was designed for simple installation

  - Single binary and embedded dashboard - no external services needed
  - Docker image with Multi-arch support
  - Supports x86-64 and ARM64 architectures -> runs fine on servers, NAS boxes and Raspberry Pi 4/5
  - Community supported Helm chart for k8s deployment

## Architecture

Denyzer is a single Go binary with everything embedded: the DNS proxy, the
resolution pipeline, an embedded SQLite store and the dashboard. No external
database, message queue or metrics service is required.

### Request pipeline

A DNS request passes through a chain of resolvers: client-name resolution,
blocking (domain, deep CNAME and IP checks), conditional forwarding, custom DNS
and hosts-file lookups, DNSSEC validation, caching and finally the upstream
resolvers. The `stats` resolver observes every request and hands a sample to the
statistics collector over a non-blocking channel (capacity 1024); when the
channel is full the sample is dropped and a rate-limited warning is logged, so
logging can never stall DNS resolution.

### Caching

The caches are backed by [`jellydator/ttlcache`](https://github.com/jellydator/ttlcache).
A cache hit does not extend the TTL: the lifetime is counted from the write.

| Cache | Key | Purpose |
| --- | --- | --- |
| Result cache | 2-byte big-endian DNS QTYPE + lower-case FQDN | DNS answers (wire format, EDNS0 OPT stripped); TTL from the answer, clamped by `minCachingTime`/`maxCachingTime`; NXDOMAIN and empty answers use `cacheTimeNegative` |
| Prefetch counters | same as the result cache | Sliding-window query count per key; hot entries are refreshed in the background when `prefetching` is enabled |
| Client names | client IP | Reverse-DNS names, one hour |
| Blocking FQDN→IP | lower-case FQDN identifier | Client-group matching for response-IP blocking |
| DNSSEC validation | client view + names + IP + FQDN | Validation results, isolated per client view |

Hits and misses are reported through callbacks, so the dashboard can show the
current number of cached entries without maintaining a second data structure.

### Query logging and statistics

Query logging is asynchronous and batched. `AsyncLogWriter` buffers entries in a
bounded channel (2000) and flushes on either `queryLog.flushInterval` or
`queryLog.batchSize`. Before writing, the batch is aggregated in memory:

- one `query_logs` row per accepted query (when `question` is listed in
  `queryLog.fields`),
- one metric delta per bucket for the global, domain and client dimensions at
  minute, hour and day granularity,
- the all-time counters and the per-query-type counters.

The query-log rows and every metric delta of a flush are committed in **one
SQLite transaction**, so the dashboard never observes a query without its
counters (or the other way around). Cleanup runs every 12 hours, and the
trailing window rollups are refreshed at start-up and then hourly.

### Storage

Denyzer stores everything in a single SQLite database (`queryLog.target`).
`queryLog.type` defaults to `sqlite`, so a normal configuration only needs the
target path; `console` and `none` are opt-outs that do not feed the dashboard.
The driver is the native `mattn/go-sqlite3`, so binaries are built with CGO; the
database runs in WAL mode with `synchronous=NORMAL`, which keeps readers
unblocked while a batch is written.

There are only two business tables:

**`query_logs`** — the detail view, ordered by an auto-increment `id` that is
also the pagination cursor. Filters (domain substring, response type, client
name or IP, query type) are applied to the newest rows first, so a page is
always a keyset scan rather than an offset scan.

**`metrics`** — every aggregated counter. One row per
`(granularity, dimension, bucket_start, entity)`:

| Granularity | Bucket | Used for |
| --- | --- | --- |
| `minute` | minute start | 1-hour charts (60 points) and 24-hour charts (144 ten-minute points) |
| `hour` | hour start | 7-day charts (168 points) and 30-day charts (720 points) |
| `day` | local midnight | retention, daily rollups |
| `all_time` | 0 | lifetime totals |
| `window_7d`, `window_30d` | 0 | dashboard Top lists |

Dimensions are `global`, `domain`, `client` and `query_type`. The primary key is
ordered `(granularity, dimension, bucket_start, entity)` and the table is
`WITHOUT ROWID`, so every hot read is a contiguous primary-key scan of the
B-tree — series, top lists and retention deletes all avoid a second lookup.
Counters are cumulative: an upsert adds the delta of the current batch to the
existing row.

The 7-day and 30-day Top lists used to require summing one day bucket per
domain, which grows linearly with traffic. Two trailing-window rollups keep one
row per domain and client instead. The writer adds each query to both windows,
and the hourly maintenance subtracts the day that leaves the window; the result
is an exact, independently ranked Top-N source that never prunes buckets by
rank. After an upgrade, or whenever the stored watermark is missing, the rollup
is rebuilt once from the day buckets; until that finishes the dashboard falls
back to the day buckets and returns the same numbers.

### Retention

`queryLog.retention.historyMonths` (1–12, default 1) controls the cleanup
window. Cleanup removes:

- `query_logs` rows older than the cutoff,
- minute and hour metric buckets older than the cutoff,
- per-domain, per-client and per-query-type day buckets older than the cutoff,

while keeping the all-time counters and the global daily metrics, so lifetime
totals and long-term history survive forever. The cleanup keeps two extra days
of day buckets beyond the 30-day window so the rollup can always subtract a day
that expires.

### Dashboard and API

The dashboard is a Next.js static export embedded in the binary and served at
`/dashboard/`. It reads the store through a small JSON API:

| Endpoint | Purpose |
| --- | --- |
| `GET /api/stats/summary` | all-time counters, cache entries, blocked ratio |
| `GET /api/stats/hourly_trend` | chart series for the selected window |
| `GET /api/stats/top_domains` | Top domains, independently ranked for All and Blocked |
| `GET /api/stats/top_clients` | Top clients, independently ranked for All and Blocked |
| `GET /api/stats/search_domains`, `GET /api/stats/search_clients` | entity search over the same rollup source |
| `GET /api/stats/query_types` | query-type breakdown |
| `GET /api/logs/query` | paginated query log |
| `POST /api/stats/refresh` | rebuild the dashboard snapshots |
| `POST /api/stats/cleanup` | purge history with a chosen retention window |

Top-N snapshots are cached per `dimension:window` and refreshed in the
background (stale-while-revalidate), so switching between Top 5/10/25/50 only
re-ranks an in-memory list. The OpenAPI/RapiDoc documentation for these endpoints
is served alongside the regular API docs.

## Acknowledgements

Denyzer would not exist without these projects:

- [Blocky](https://github.com/0xERR0R/blocky) - the DNS proxy and ad-blocker by
  [0xERR0R](https://github.com/0xERR0R) that Denyzer is based on, and all its
  contributors.
- [blocky-ui](https://github.com/GabeDuarteM/blocky-ui) - the web dashboard by
  [GabeDuarteM](https://github.com/GabeDuarteM) that Denyzer's dashboard grew
  out of, and all its contributors.

Thanks to both projects and to everyone who contributed to them.
