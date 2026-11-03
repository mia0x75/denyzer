# Interfaces

## REST API


??? abstract "OpenAPI specification"

    ```yaml
    --8<-- "api/openapi.yaml"
    ```

The HTTPS management listener (`ports.admin`) provides the REST API. The public
HTTP, HTTPS and HTTP/3 DoH listeners do not expose it. You can download the
[OpenAPI YAML](api/openapi.yaml) interface specification.

You can also browse the interactive API documentation (RapiDoc) documentation [online](rapidoc.html).

### Authentication

Configure the single administrator with `denyzer auth --username admin`
([configuration details](configuration.md#management-authentication)). Open
`https://<denyzer-host>:<admin-port>/login/` to log in; the dashboard is at `/`
on the same HTTPS origin. Use a trusted certificate matching this hostname/IP.

`GET /api/login` returns `{"enabled":true}` or `{"enabled":false}` without
authentication. When the request carries a valid page cookie it also returns that
session's `token`, so a second tab or a restarted browser continues the same
session. `POST /api/login` accepts `application/json` with `username` and
`password`, and returns `token` and `expiresAt`. Wrong usernames and passwords
both return `401 invalid credentials`.

Include `Authorization: Bearer <token>` on all other API and debug requests.
`POST /api/logout` revokes the current token, clears the page cookie and returns
`204`. Tokens expire seven days after login without renewal, and all tokens are
invalidated by a password reset or server restart. Cookies authenticate pages
only; the dashboard keeps its API token in `sessionStorage` and clears it before
returning to `/login/` on a `401`. Transport failures, HTTP server errors and
cancelled requests keep the session: a network blip does not sign the
administrator out, and a regenerated self-signed certificate surfaces as a
connection error telling the user to reload. Cross-origin management requests are rejected with `403`.
An unreadable or invalid auth configuration fails closed with `503`.

Authentication can be disabled only for explicit loopback management binds,
with a startup warning. Public DoH remains unauthenticated and separate.

### Common endpoints

| Method | Path                  | Purpose                                              |
| ------ | --------------------- | ---------------------------------------------------- |
| GET    | `/api/login`            | Check whether authentication is enabled.           |
| POST   | `/api/login`            | Create a seven-day administrator session.          |
| POST   | `/api/logout`           | Revoke the current Bearer session.                 |
| POST   | `/api/blocking/enable`  | Enable blocking globally.                          |
| POST   | `/api/blocking/disable` | Disable blocking globally (optional `duration`, `groups` query params). |
| GET    | `/api/blocking/status`  | Return current blocking status as JSON.            |
| POST   | `/api/lists/refresh`    | Refresh all allow/denylists.                       |
| POST   | `/api/cache/flush`      | Clear the entire DNS response cache.               |
| POST   | `/api/query`            | Run a DNS query through Denyzer and return the result as JSON. |
| GET    | `/api/stats`            | In-memory DNS statistics over a rolling 24h window as JSON. Requires [statistics](configuration.md#statistics) to be enabled; returns `503` otherwise. |

!!! example "Flush the DNS cache"

    ```sh
    curl -X POST https://<denyzer-host>:<admin-port>/api/cache/flush \
      -H "Authorization: Bearer $DENYZER_API_TOKEN"
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
    - A [DNSSEC](configuration.md#dnssec-validation) validation failure (`BOGUS`) is a SERVFAIL — denyzer
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

Denyzer's management CLI uses the HTTPS REST API and takes its Bearer token from
`DENYZER_API_TOKEN`. Set it to the `token` returned by `POST /api/login`.
The CLI verifies the management certificate; there is no insecure TLS flag.
Use `--apiHost` and `--apiPort` to select the management host and `ports.admin`
port, not a DoH listener.

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
- `./denyzer auth --username admin` sets or resets credentials locally, without a running server
- `./denyzer version` prints only Version, Build time and Architecture

!!! tip 

    Pass the token into a running container with
    `docker exec -e DENYZER_API_TOKEN denyzer ./denyzer blocking status`.
    Its configured management host must also match a trusted certificate.
