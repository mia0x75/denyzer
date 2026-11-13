# Installation

You can choose one of the following installation options:

* Run as standalone binary
* Run as docker container

## Prepare your configuration

Denyzer supports single or multiple YAML files as configuration. Create new `config.yml` with your configuration
(see [Configuration](configuration.md) for more details and all configuration options).

Simple configuration file, which enables only basic features:

```yaml
upstreams:
  groups:
    default: # (1)!
      - 46.182.19.48
      - 80.241.218.68
      - tcp-tls:fdns1.dismail.de:853
      - https://dns.digitale-gesellschaft.ch/dns-query
blocking:
  denylists:
    ads: # (2)!
      - https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts
  clientGroupsBlock:
    default:
      - ads
ports:
  dns: 53 # (3)!
  admin: 4000
```

1.  Upstream DNS resolvers denyzer forwards allowed queries to. Plain IPs as well as `tcp-tls:` (DoT) and `https://` (DoH) endpoints are supported.
2.  A named denylist group. Each entry is a URL or local file with hosts/domains to block; reference it from `clientGroupsBlock`.
3.  Listening ports — `dns` serves DNS (UDP/TCP), `admin` serves the HTTPS management API and dashboard. Optional `http`/`https` ports serve DoH only.

### Set the administrator password

After downloading the binary, set credentials before starting this configuration:

```sh
./denyzer auth --config config.yml --username admin
```

The command prompts twice without echoing the password and atomically writes an
Argon2id hash with file permissions `0600`. Use the same command to reset the
password. The server user must be able to read the resulting file; the CLI user
needs write access to its directory. Docker users can use the one-off command
below instead.

The dashboard is at `https://<denyzer-host>:4000/`, with login at `/login/`.
Configure `certFile` and `keyFile` with a trusted certificate matching this
hostname/IP. The generated self-signed certificate is not trusted by default,
may not match the access address, and is rejected by the CLI. See
[Management authentication](configuration.md#management-authentication).

## Run Denyzer

=== "Standalone binary"

    Download the binary file from [GitHub](https://github.com/mia0x75/denyzer/releases) for your architecture and
    run `./denyzer --config config.yml`.

    !!! warning

        To bind a **privileged port (< 1024, e.g. 53 or 853)** as a non-root user on
        Linux, the binary needs the `NET_BIND_SERVICE` capability. Add it with
        `setcap 'cap_net_bind_service=+ep' ./denyzer`, run as root (not recommended),
        or configure a port >= 1024.

        If Denyzer runs under a **restricted capability bounding set** (for example a
        hardened `systemd` unit, or a container that drops capabilities), use
        `setcap 'cap_net_bind_service=+p' ./denyzer` instead. Denyzer raises the
        capability to effective itself at startup, which avoids the
        `operation not permitted` exec error that `+ep` triggers when the capability
        is not in the bounding set.

=== "Docker"

    Keep the configuration in a directory so the running container can see
    atomic credential replacements. Prepare credentials as the image's UID 100:

    ```sh
    mkdir -p config
    cp config.yml config/config.yml
    sudo chown 100:100 config config/config.yml
    docker run --rm -it -v "$PWD/config:/config" ghcr.io/mia0x75/denyzer \
      auth --config /config --username admin
    ```

    Rerun the same one-off auth command to reset a running container's password.
    The server can keep the configuration directory mounted read-only; the
    one-off auth container needs the writable mount shown above. Do not mount a
    single config file for live resets: replacing it on the host can leave the
    running container reading the old inode until the container is recreated.

    !!! note "Running under a restricted runtime"

        The container image is capability-aware. The binary ships with
        `cap_net_bind_service=+p` (permitted only), so the container starts even under
        a restricted runtime that drops all capabilities (for example the Kubernetes
        *Restricted* Pod Security Standard, `capabilities: drop: [ALL]`).

        - On **ports >= 1024** no capability is required.
        - On **privileged ports (< 1024, e.g. 53/853)** Denyzer needs `NET_BIND_SERVICE`.
          With the default runtime capability set it is already present and Denyzer
          enables it automatically. Under `drop: [ALL]`, grant it back (Kubernetes
          `securityContext.capabilities.add: ["NET_BIND_SERVICE"]`, or
          `docker run --cap-add NET_BIND_SERVICE`) or configure a port >= 1024.

    !!! note "Writable mounts and file permissions"

        The container runs as the unprivileged user **UID 100**. Any directory Denyzer
        writes to - the query log target, or `loading.downloads.cachePath` - must be
        writable by that user, otherwise Denyzer logs a `permission denied` error and
        continues without writing.

        The image ships `/app/cache` and `/logs` pre-created and owned by UID 100, so
        mounting a **named volume** over either path works with no host-side setup:

        ```sh
        docker run -v denyzer_cache:/app/cache ... ghcr.io/mia0x75/denyzer
        ```

        A **bind mount** always keeps the host directory's ownership, so it needs one of:

        - `chown -R 100:100 /path/on/host` before starting the container, or
        - run as the host user that owns the directory:
          `docker run -u "$(id -u):$(id -g)" ...`, or in compose
          `user: "1000:1000"`.

        Configuration and TLS private-key files must also be readable by UID 100.
        The auth command writes credentials with mode `0600`, so preserve the
        container user's ownership rather than making them world-readable.

    Denyzer Docker images are published to the [GitHub Container Registry](https://ghcr.io/mia0x75/denyzer).

    === "docker run"

        Execute the following command from the command line:

        ```sh
        docker run --name denyzer -v /path/to/config:/config:ro \
          -e DENYZER_CONFIG_FILE=/config -p 127.0.0.1:4000:4000 \
          -p 53:53/tcp -p 53:53/udp ghcr.io/mia0x75/denyzer
        ```

    === "docker-compose"

        Create the following `docker-compose.yml` file:

        ```yaml
        version: "2.1"
        services:
          denyzer:
            image: ghcr.io/mia0x75/denyzer
            container_name: denyzer
            restart: unless-stopped
            # Optional the instance hostname for logging purpose
            hostname: denyzer-hostname
            ports:
              - "53:53/tcp"
              - "53:53/udp"
              - "127.0.0.1:4000:4000/tcp" # HTTPS management; change the host bind for trusted LAN access
            environment:
              - DENYZER_CONFIG_FILE=/config
              - TZ=Europe/Berlin # Optional to synchronize the log timestamp with host
            volumes:
              # Optional to synchronize the log timestamp with host
              - /etc/localtime:/etc/localtime:ro
              # config directory, prepared with the auth command above
              - ./config:/config:ro
        ```

        Then start the container with:

        ```sh
        docker-compose up -d
        ```

## Container configuration file

You can define the location of the config file in the container with environment variable `DENYZER_CONFIG_FILE`.
Using this environment variable a folder containing configuration files can also be specified.
Default value is `/app/config.yml`.

!!! note "Legacy: `CONFIG_FILE`"

    Older Denyzer versions used `CONFIG_FILE` instead of
    `DENYZER_CONFIG_FILE`. The legacy name is still accepted as a
    fallback when `DENYZER_CONFIG_FILE` is unset. New deployments
    should use `DENYZER_CONFIG_FILE`.

## Advanced docker-compose setup

The following example stores SQLite query history in a local Docker volume and mounts local
allowlist and denylist directories. Set `queryLog.target` to `/logs/denyzer.db` in the configuration;
list paths can use `/app/allowlists/allowlist.txt`.

!!! warning

    Keep the active database on a local filesystem, not on SAMBA/CIFS, NFS or other network shares.
    SQLite WAL requires shared memory and reliable file locking on the same host.
    Use network shares only for completed backups made through SQLite's backup facilities or while
    Denyzer is stopped. See [SQLite query log](configuration.md#sqlite-query-log).

!!! example

    ```yaml
    version: "2.1"
    services:
      denyzer:
        image: ghcr.io/mia0x75/denyzer
        container_name: denyzer
        restart: unless-stopped
        ports:
          - "53:53/tcp"
          - "53:53/udp"
          - "127.0.0.1:4000:4000/tcp" # HTTPS management API and dashboard
        environment:
          - DENYZER_CONFIG_FILE=/config
          - TZ=Europe/Berlin
        volumes:
          # config directory, including administrator credentials
          - ./config:/config:ro
          # Keep the active SQLite database on local storage.
          - queryLogs:/logs
          # put your custom allow/denylists in these directories
          - ./denylists:/app/denylists/
          - ./allowlists:/app/allowlists/

    volumes:
      queryLogs:
        driver: local
    ```

    !!! note

        The `./denylists` and `./allowlists` bind mounts above are only read by Denyzer,
        so they need no ownership changes. See the permission note under
        [Run Denyzer](#run-denyzer) if you add a writable bind mount.

## Multiple configuration files

For complex setups, pass a folder containing YAML files. Denyzer recursively
merges them in lexical path order; later scalar and list values override earlier
ones. See [Multiple configuration files](configuration.md#multiple-configuration-files).

```sh
./denyzer --config ./config/
```

!!! warning

    Duplicate keys within a single YAML document are rejected. In directory mode,
    `denyzer auth --config ./config/ --username admin` updates the last YAML file.

## Other installation types

!!! warning

    These projects are not associated with Denyzer devs and are listed here for convenience.

### Arch Linux via AUR

See [https://aur.archlinux.org/packages/denyzer/](https://aur.archlinux.org/packages/denyzer/)

### Alpine Linux

See [https://pkgs.alpinelinux.org/packages?name=denyzer&branch=edge&repo=&arch=](https://pkgs.alpinelinux.org/packages?name=denyzer&branch=edge&repo=&arch=)

### CentOS/Debian/Fedora install script

See [https://github.com/m0zgen/denyzer-installer](https://github.com/m0zgen/denyzer-installer)

### FreeBSD

See [https://www.freebsd.org/cgi/ports.cgi?query=denyzer&stype=all](https://www.freebsd.org/cgi/ports.cgi?query=denyzer&stype=all)

### Gentoo

See the [Gentoo Wiki](https://wiki.gentoo.org/wiki/Project:GURU/Information_for_End_Users) to enable the GURU repository, then run `emerge net-dns/denyzer`.

### NixOS

Add `pkgs.denyzer` as a module:

```nix
services.denyzer = {
  enable = true;

  settings = {
    # anything from config.yml
  };
};
```

### macOS via Homebrew

See [https://formulae.brew.sh/formula/denyzer](https://formulae.brew.sh/formula/denyzer)

### TrueNAS SCALE via TrueCharts

See [https://truecharts.org/charts/enterprise/denyzer/](https://truecharts.org/charts/enterprise/denyzer/)
(TrueCharts is not an official TrueNAS project)

## Companion projects

!!! warning

    These projects are not associated with Denyzer devs and are listed here for convenience.

### Lists updater

[Denyzer lists updater](https://github.com/shizunge/denyzer-lists-updater) updates list related configuration without restarting denyzer DNS.

### Web UIs

- [Denyzer Frontend](https://github.com/Mozart409/denyzer-frontend) provides a Web UI to control denyzer.
See linked project for installation instructions.

- [DenyzerUI](https://github.com/GabeDuarteM/denyzer-ui) provides a fully featured and modern Web UI for managing your Denyzer DNS server.

- [Denyzer Visor](https://github.com/JCHHeilmann/denyzer-visor) is a static SPA dashboard with live metrics and DNS query testing. Comes with an optional Go sidecar that adds historical analytics based on parsing Denyzer's log files, log viewing, and config editing.
