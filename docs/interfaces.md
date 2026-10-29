# Interfaces

## REST API


??? abstract "OpenAPI specification"

    ```yaml
    --8<-- "api/openapi.yaml"
    ```

If http listener is enabled, denyzer provides REST API. You can download the [OpenAPI YAML](api/openapi.yaml) interface specification. 

You can also browse the interactive API documentation (RapiDoc) documentation [online](rapidoc.html).

### Common endpoints

| Method | Path                  | Purpose                                              |
| ------ | --------------------- | ---------------------------------------------------- |
| GET    | `/api/blocking/enable`  | Enable blocking globally.                          |
| GET    | `/api/blocking/disable` | Disable blocking globally (optional `duration`, `groups` query params). |
| GET    | `/api/blocking/status`  | Return current blocking status as JSON.            |
| POST   | `/api/lists/refresh`    | Refresh all allow/denylists.                       |
| POST   | `/api/cache/flush`      | Clear the entire DNS response cache.               |
| POST   | `/api/query`            | Run a DNS query through Denyzer and return the result as JSON. |
| GET    | `/api/stats`            | In-memory DNS statistics over a rolling 24h window as JSON. Requires [statistics](configuration.md#statistics) to be enabled; returns `503` otherwise. |

!!! example "Flush the DNS cache"

    ```sh
    curl -X POST http://<denyzer-host>:<http-port>/api/cache/flush
    ```

    Returns HTTP `200` on success. Useful after editing `customDNS`
    or `hostsFile` entries that may already be cached.

!!! note "Statistics semantics"

    For `/api/stats`, the `summary` fields are server-computed categories, so callers never
    interpret a raw response type:

    | Field      | Response types                                        |
    | ---------- | ----------------------------------------------------- |
    | `blocked`  | `BLOCKED` + `REBIND`                                  |
    | `filtered` | `FILTERED` + `NOTFQDN`                                |
    | `forwarded`| `RESOLVED` + `CONDITIONAL`                            |
    | `cached`   | `CACHED`                                              |
    | `local`    | `CUSTOMDNS` + `HOSTSFILE` + `SPECIAL` + `SYNTHESIZED` |
    | `errors`   | `BOGUS`, plus queries a resolver failed outright      |

    `blocked` counts only queries blocked to protect the client: denylist hits and
    [DNS rebinding](configuration.md#dns-rebinding-protection) hits. Two other outcomes are
    deliberately kept out of it, so they cannot inflate `blocked` or the `topBlockedDomains`
    list:

    - Query-type filtering (e.g. `AAAA` via `filtering.queryTypes`) and non-FQDN rejections are
      client-requested, not protective, and are counted as `filtered`.
    - A [DNSSEC](configuration.md#dnssec) validation failure (`BOGUS`) is a SERVFAIL — denyzer
      could not obtain a trustworthy answer — so it is a resolution error, not a block, and is
      counted as `errors` together with queries a resolver failed outright.

    The `blocked` / `filtered` split also applies to the `perHour` series, which carries both.
    Together with `dropped` (rate-limited), the categories above partition every query, so they
    always add up to `queries`.

    The `lists` and `cache` objects are point-in-time gauges
    (current values, not affected by the 24h window), while `start`/`end` bound the windowed fields
    only. All timestamps (`start`, `end`, `perHour[].hour`) are always returned in UTC (RFC 3339,
    `Z` suffix), regardless of the server's local time zone. Statistics are served as plain JSON.

## CLI

Denyzer provides a CLI interface to control. This interface uses internally the REST API.

To run the CLI, please ensure, that denyzer DNS server is running, then execute `denyzer help` for help or

- `./denyzer blocking enable` to enable blocking
- `./denyzer blocking disable` to disable blocking
- `./denyzer blocking disable --duration [duration]` to disable blocking for a certain amount of time (30s, 5m, 10m30s,
  ...)
- `./denyzer blocking disable --groups ads,othergroup` to disable blocking only for special groups
- `./denyzer blocking status` to print current status of blocking
- `./denyzer query <domain>` execute DNS query (A) (simple replacement for dig, useful for debug purposes)
- `./denyzer query <domain> --type <queryType>` execute DNS query with passed query type (A, AAAA, MX, ...)
- `./denyzer lists refresh` reloads all allow/denylists
- `./denyzer stats` shows DNS statistics (requires `statistics.enable: true`)
- `./denyzer validate [--config /path/to/config.yaml]` validates configuration file

!!! tip 

    To run this inside docker run `docker exec denyzer ./denyzer blocking status`
