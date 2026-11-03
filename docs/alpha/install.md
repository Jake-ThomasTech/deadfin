# Install Deadfin (community alpha)

For container hardening, Kubernetes, Podman, NAS, backups, and production
rollout checks, see the [deployment and security guide](deployment-security.md).

Deadfin is a Go server that speaks the Jellyfin 12.0.0 API. Stock Jellyfin
clients connect to it as they connect to Jellyfin. This page tells you how to
run a new, empty Deadfin server. To move an existing Jellyfin server, read
[importing-from-jellyfin.md](importing-from-jellyfin.md) after you finish
this page.

Alpha means: back up your media metadata, expect defects, and keep your
Jellyfin server available. Deadfin never writes to your media folders unless
you enable a feature that saves files next to media.

## What you need

- A Linux host with Docker or Podman (amd64 or arm64).
- Your media on a path the container can read.
- A TMDb v3 API key, free at
  [themoviedb.org/settings/api](https://www.themoviedb.org/settings/api).
  Movie and TV metadata and artwork come from TMDb, and each server uses its
  own key. Without one, movies and shows keep their file names and get no
  artwork (divergence D-11). Set it before the first library scan; a key
  added later takes effect after a metadata refresh of the library.

## Image

```
ghcr.io/deadfin-org/deadfin:alpha
```

`:alpha` follows the newest release or release candidate. `:latest` follows
the newest release (no `-rc.N`); the first one is v0.3.0. To control
upgrades, pin a tag from the
[tags page](https://github.com/deadfin-org/deadfin/tags), for example
`ghcr.io/deadfin-org/deadfin:v0.3.1`. Each tag is a multi-arch image
(amd64 and arm64) with Jellyfin's FFmpeg build at `/usr/lib/jellyfin-ffmpeg`.

The image is public. It ships the license texts of its third-party components
(see the README, §License).

## Volumes and paths

| Container path | Mode | Holds |
|---|---|---|
| `/var/lib/deadfin` | read-write | Everything Deadfin owns except a PostgreSQL database: `config/`, `data/deadfin.db` (SQLite), `metadata/`, `cache/` (transcodes, images), `log/`, `root/` (library definitions). This is `DEADFIN_DATA_DIR`. Back it up. |
| `/var/lib/deadfin/web` | read-only, optional | A web client that replaces the bundled deadfin-web, for example jellyfin-web 12.0. Deadfin serves `/web` from `<DEADFIN_DATA_DIR>/web` when that directory holds an `index.html`. See [Web client](#web-client). |
| `/media` (your choice) | read-only | Your media. Mount read-write only if you want subtitle uploads, or images and NFO saved next to media. |

The container runs as UID/GID 65532. The data directory must be writable by
that user.

Use local storage for `/var/lib/deadfin`. SQLite on NFS or SMB can corrupt
the database. Media on NFS is fine.

## Environment variables

Set only what you need. All are optional.

| Variable | Default | Purpose |
|---|---|---|
| `DEADFIN_DATA_DIR` | unset (the image sets `JELLYFIN_DATA_DIR=/var/lib/deadfin`) | Data root. Leave it unset in the container to use `/var/lib/deadfin`. `JELLYFIN_DATA_DIR` is the accepted fallback when `DEADFIN_DATA_DIR` is not set. `JELLYFIN_CONFIG_DIR`, `JELLYFIN_CACHE_DIR`, `JELLYFIN_LOG_DIR` and `JELLYFIN_WEB_DIR` have no effect; Deadfin logs a warning for each one that is set. |
| `DEADFIN_DATABASE_URL` | empty: SQLite | A `postgres://` or `postgresql://` URL selects PostgreSQL (see [Database](#database)). The `--database-url` flag overrides it. |
| `TMDB_API_KEY` | unset | TMDb v3 key. Without it, movies and series keep their file and folder names and get no remote art. |
| `AUDIODB_API_KEY` | unset | TheAudioDB key for music artwork and bios. |
| `LISTENBRAINZ_TOKEN`, `LISTENBRAINZ_SERVER` | unset | ListenBrainz scrobbling. |
| `DEADFIN_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `DEADFIN_SQLITE_CACHE_MB`, `DEADFIN_SQLITE_MMAP_MB` | SQLite defaults | SQLite page cache and mmap size. Raise them for libraries with tens of thousands of items. |
| `DEADFIN_PPROF_ADDR` | unset | Go profiler address. Use a loopback address only, for example `127.0.0.1:6060`. |

The server listens on TCP 8096 (`--listen` changes it) and answers client
discovery on UDP 7359.

## Database

Deadfin stores its library, users and watch state in SQLite or PostgreSQL.

- **SQLite** is the default. Leave `DEADFIN_DATABASE_URL` empty and Deadfin
  keeps one file, `data/deadfin.db`, under the data directory.
- **PostgreSQL** (tested on 17): set `DEADFIN_DATABASE_URL` to a connection URL,
  for example `postgres://deadfin:secret@postgres:5432/deadfin?sslmode=disable`.
  The user needs the `CREATE` privilege on the schema Deadfin uses
  (`public` unless the URL sets `search_path`); the database owner has it.
  Deadfin creates its tables at the first start and
  migrates them at each upgrade. To keep Deadfin in its own schema, create
  the schema and add `search_path=<schema>` to the URL.

The startup log names the database in use, with the password hidden:

```
application: store PostgreSQL postgres://deadfin:xxxxx@postgres:5432/deadfin?sslmode=disable
```

Choose the database when you install. A server that changes the setting
later starts with an empty database. On PostgreSQL, back up the database
(`pg_dump`) and the data directory, which keeps config, metadata, images and
library definitions.

## Web client

The image includes deadfin-web, the default web client, in
`/usr/share/deadfin/web`. Deadfin serves it at `/web/`. Each Deadfin release
pins one deadfin-web release; the startup log line `web client` names the
directory and the version in use, and `/web/build.json` names the version.

**deadfin-web** (recommended, alpha). It covers first-run setup, browsing,
TV series, search, playback with track selection, and Quick Connect. It has
no admin dashboard after setup.

**jellyfin-web 12.0** (stock, complete, with the admin dashboard). To use
jellyfin-web instead, put its files in `./deadfin/web` and mount that
directory at `/var/lib/deadfin/web`. Copy the files out of the stock Jellyfin
image that Deadfin's tests use (tag `12.0-rc2`, pinned by digest) once:

```bash
mkdir -p ./deadfin/web
cid=$(docker create jellyfin/jellyfin@sha256:2d1a69be5aa2695d0c2df90ebeb76bbcc6efc0a798cb09bfa65906680e1d785a)
docker cp "$cid":/jellyfin/jellyfin-web/. ./deadfin/web/
docker rm "$cid"
```

```yaml
    volumes:
      - ./deadfin/data:/var/lib/deadfin
      - ./deadfin/web:/var/lib/deadfin/web:ro
```

When `<DEADFIN_DATA_DIR>/web` holds an `index.html`, Deadfin serves that
directory in place of the bundled client. The same mount runs a different
deadfin-web release: extract its tarball from the
[releases page](https://github.com/deadfin-org/deadfin-web/releases) into
`./deadfin/web`. A `web` directory without an `index.html` has no effect;
Deadfin logs a warning and serves the bundled client.

## Run it

```yaml
# compose.yaml
services:
  deadfin:
    image: ghcr.io/deadfin-org/deadfin:alpha
    restart: unless-stopped
    ports:
      - "8096:8096"
      - "7359:7359/udp"
    environment:
      TMDB_API_KEY: "your-key"   # movie and TV metadata and art
    volumes:
      - ./deadfin/data:/var/lib/deadfin
      - /srv/media:/media:ro
```

```bash
mkdir -p ./deadfin/data && sudo chown 65532:65532 ./deadfin/data
docker compose up -d
docker compose logs -f deadfin
```

### Run it on PostgreSQL

Add a `postgres` service and point Deadfin at it:

```yaml
# compose.yaml
services:
  deadfin:
    image: ghcr.io/deadfin-org/deadfin:alpha
    restart: unless-stopped
    depends_on:
      postgres:
        condition: service_healthy
    ports:
      - "8096:8096"
      - "7359:7359/udp"
    environment:
      DEADFIN_DATABASE_URL: "postgres://deadfin:change-me@postgres:5432/deadfin?sslmode=disable"
      TMDB_API_KEY: "your-key"   # movie and TV metadata and art
    volumes:
      - ./deadfin/data:/var/lib/deadfin
      - /srv/media:/media:ro
  postgres:
    image: postgres:17
    restart: unless-stopped
    environment:
      POSTGRES_USER: deadfin
      POSTGRES_PASSWORD: change-me
      POSTGRES_DB: deadfin
    volumes:
      - ./deadfin/postgres:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U deadfin -d deadfin"]
      interval: 5s
      timeout: 5s
      retries: 10
```

The CLI commands, for example `docker compose run --rm deadfin add-library`,
read `DEADFIN_DATABASE_URL` from the same service and use the same database.

## First-run setup and the setup link

Until you finish the startup wizard, Deadfin serves the wizard only to
clients that reach it **directly from the local network**. This stops a
stranger from taking over a new server that is reachable from the internet
(divergence D-53).

- On the same LAN, with no proxy between you and Deadfin: open
  `http://<host>:8096/web/` and complete the wizard.
- Through a reverse proxy, a VPN from another network, or any request that
  carries `Forwarded`, `X-Forwarded-For` or `X-Real-IP`: the wizard gets
  `403 First-run setup is open only to the local network or with the setup
  link in the server log.` Find this line in the log:

  ```
  level=WARN msg="first-run setup is pending: ..." path="/deadfin/v1/setup?token=<64 hex digits>"
  ```

  Open `https://<your-public-url>/deadfin/v1/setup?token=<token>` once in the
  browser you will use. Deadfin sets a cookie and sends you to `/web/`. The
  token changes at every restart while setup is pending.

- Docker Desktop, Colima and rootless Podman can present every client as a
  local address. Then the locality test admits everyone, and only the token
  protects the wizard. Do not publish such a server to the internet before
  you finish the wizard.

After the wizard, the gate is off and the log line goes away.

### Add libraries

Add libraries in the wizard, or later in Dashboard → Libraries. You can also
add them with the API or the CLI:

```bash
# API (an admin token or API key in the Authorization header)
curl -X POST "http://<host>:8096/Library/VirtualFolders?name=Movies&collectionType=movies&paths=/media/movies&refreshLibrary=true" \
     -H 'Authorization: MediaBrowser Token="<token>"' -H 'Content-Type: application/json' \
     -d '{"LibraryOptions":{"Enabled":true}}'

# CLI, with the server stopped
docker compose run --rm deadfin add-library --name Movies --type movies --path /media/movies
```

With the CLI the type goes in `--type`. `collectionType` is `movies`, `tvshows`, `music`, `musicvideos`,
`homevideos`, `boxsets` or `books`. Leave it out for a mixed movies-and-shows
library.

## Reverse proxy

Any proxy that works for Jellyfin works for Deadfin. Forward WebSockets on
`/socket`, and do not buffer `/Videos/` and `/Audio/` responses.

- Use the setup link above for the first run.
- List the proxy's address or subnet in `KnownProxies` (Dashboard →
  Networking, or `network.xml`) before the first start. The one-time setup
  cookie is then `Secure` when the proxy sends `X-Forwarded-Proto: https`
  (D-53).
- Client identity stays the proxy's address: every client behind the proxy
  shares it. This affects the dashboard's client addresses, the
  local-network bitrate rules, and the QuickConnect limit of 16 open
  requests per address (D-55).
- To serve Deadfin under a path such as `/jellyfin`, set the base URL in
  Dashboard → Networking and restart.

Example (Caddy):

```
media.example.com {
    reverse_proxy deadfin:8096
}
```

## Hardware transcoding

Software transcoding works on any host. Hardware paths exist for Intel QSV
and VAAPI (iHD driver) and NVIDIA NVENC on Linux (divergence D-37). Pass the
device through as for Jellyfin (`/dev/dri`, or the NVIDIA runtime) and choose
the type in Dashboard → Playback → Transcoding. Other types fall back to
software.

## Upgrade and roll back

- Upgrade: on `:alpha`, run `docker compose pull` and restart. On a pinned
  tag, change the tag and restart. Deadfin migrates the database at
  start. Back up `/var/lib/deadfin` first. The alpha does not support
  downgrades after a migration.
- Roll back to Jellyfin: stop Deadfin and start your untouched Jellyfin. The
  importer never writes to Jellyfin's files.

## Check that it runs

```bash
curl -s http://<host>:8096/System/Info/Public
curl -s http://<host>:8096/deadfin/v1/build
```

The first answers like Jellyfin. The second names the Deadfin version and
revision; include it in every bug report.
