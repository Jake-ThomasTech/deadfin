<p align="center">
  <img src="docs/assets/deadfin.svg" alt="Deadfin" width="128" height="128">
</p>

<h1 align="center">Deadfin</h1>

<p align="center">
  <strong>Our Go port of Jellyfin.</strong><br>
  Use your Jellyfin apps with Deadfin. Most familiar flows work unchanged; see
  the <a href="docs/alpha/client-matrix.md">client matrix</a> for tested paths and known alpha gaps.
</p>

<p align="center">
  <a href="LICENSE"><img alt="License: GPL-2.0-only" src="https://img.shields.io/badge/license-GPL--2.0--only-blue"></a>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/github/go-mod/go-version/deadfin-org/deadfin"></a>
  <a href="https://github.com/deadfin-org/deadfin/tags"><img alt="Latest release" src="https://img.shields.io/github/v/tag/deadfin-org/deadfin?sort=semver&label=release"></a>
  <a href="https://github.com/deadfin-org/deadfin/actions/workflows/ci.yml"><img alt="CI status" src="https://github.com/deadfin-org/deadfin/actions/workflows/ci.yml/badge.svg?branch=master"></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/deadfin-org/deadfin"><img alt="OpenSSF Scorecard" src="https://api.scorecard.dev/projects/github.com/deadfin-org/deadfin/badge"></a>
</p>

---

Deadfin is a media server written in Go. It serves the Jellyfin 12.0.0 HTTP
and WebSocket API, so stock Jellyfin clients connect to it the same way they
connect to Jellyfin. Point jellyfin-web, Swiftfin, Jellyfin for Android TV or
Infuse at it, and browse and play your library. The
[client matrix](docs/alpha/client-matrix.md) shows what each app was tested
with and the known alpha gaps.

## Why Deadfin

- **One Go server process.** No .NET runtime. The release image is
  multi-arch (amd64 and arm64) and carries everything it needs, including
  Jellyfin's own FFmpeg build.
- **Works with the Jellyfin apps you already have.** jellyfin-web 12.0,
  Swiftfin and Infuse are tested with the real apps; Jellyfin for Android TV
  with its own requests. See [Clients](#clients).
- **Hardware transcoding.** Intel QSV and VAAPI (iHD driver) and NVIDIA
  NVENC on Linux, with HDR10, HLG and Dolby Vision tone mapping. Software
  transcoding works on any host.
- **Moves in from Jellyfin.** A one-way importer brings across users and
  passwords, device sign-ins, watch state, playlists, collections and
  library definitions. It only reads Jellyfin's files, so your Jellyfin
  server stays as it was.
- **Small touches.** Your audio and subtitle choice carries from one episode
  to the next. Anime search also finds English, romaji and native titles
  from AniList.
- **SQLite or PostgreSQL.** SQLite is the default and needs no setup: one
  database file under the data directory. Set `DEADFIN_DATABASE_URL` to run
  on PostgreSQL instead.

## Status: alpha

Deadfin is alpha software. Back up your data, expect defects, and keep your
Jellyfin server around while you try it.

**Works today:** first-run setup, sign-in and QuickConnect, library scans
with TMDb, TheAudioDB and AniList metadata, browsing and search, direct play,
HLS transcoding, external and embedded subtitles (including burn-in), resume
and played state, playlists and collections, music, trickplay previews, and
the Jellyfin importer.

**Not available yet:** Live TV and DVR, SyncPlay, plugins, and DLNA. Some
dashboard pages (devices, logs, NFO settings) show errors. Live TV and
SyncPlay are on the [roadmap](ROADMAP.md).

Where Deadfin behaves differently from Jellyfin on purpose, the difference is
recorded in [docs/divergences.md](docs/divergences.md). The plain-language
version is [docs/alpha/known-differences.md](docs/alpha/known-differences.md).

New to Deadfin? The [FAQ](docs/alpha/faq.md) answers the first questions:
missing artwork, HDR transcodes, the web client, GPUs and reverse proxies.
All guides are listed in the [documentation index](docs/README.md).

## Choose your client

Use your Jellyfin apps with Deadfin. Pick a web interface for the browser and
for server setup, then add the apps you already use.

| Client | Maturity | Install | Biggest gap |
|---|---|---|---|
| [deadfin-web](https://github.com/deadfin-org/deadfin-web) | Recommended. Experimental alpha. | In the image. Deadfin serves it at `/web/`. | No admin dashboard after first-run setup. For users, library changes and server settings, use jellyfin-web instead. |
| jellyfin-web 12.0 | Stock and complete: every page, including the dashboard. | [Use jellyfin-web instead](#use-jellyfin-web-instead): put its files in `./deadfin/web`. | Some dashboard pages (devices, logs, NFO settings) show errors. |
| Jellyfin apps: Swiftfin, Infuse, Jellyfin for Android and Android TV, and others | Unchanged stock apps. | Add the server by its URL, as for Jellyfin. | Per app and per path: see the [client matrix](docs/alpha/client-matrix.md). |
| [SharkParty](https://github.com/deadfin-org/SharkParty) | Pre-alpha tvOS app, a Swiftfin fork. | Build from source with Xcode. | No App Store or TestFlight build. |

## Quick start

Deadfin listens on TCP **8096** and answers client discovery on UDP
**7359**. All its state lives in `/var/lib/deadfin` in the container. The
container runs as UID/GID 65532.

**1. Get a TMDb API key.** Movie and TV metadata and artwork come from
[TMDb](https://www.themoviedb.org/). Each Deadfin server uses its own key;
the image carries no shared one. A free v3 key takes a few minutes at
[themoviedb.org/settings/api](https://www.themoviedb.org/settings/api).
Without it, movies and shows show their file names and no artwork
(see D-11 in [known differences](docs/alpha/known-differences.md)).

**2. Make the data directory.** The container runs as UID/GID 65532:

```bash
mkdir -p ./deadfin/data
sudo chown 65532:65532 ./deadfin/data
```

The image includes [deadfin-web](https://github.com/deadfin-org/deadfin-web),
the web client. To use jellyfin-web instead, see
[Use jellyfin-web instead](#use-jellyfin-web-instead).

**3. Run Deadfin.** `:alpha` follows the newest release or release
candidate. To control upgrades, put a tag from the
[tags page](https://github.com/deadfin-org/deadfin/tags) in its place, for
example `ghcr.io/deadfin-org/deadfin:v0.3.1`.

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
      TMDB_API_KEY: "your-key"   # from step 1: movie and TV metadata and art
    volumes:
      - ./deadfin/data:/var/lib/deadfin
      - /srv/media:/media:ro
```

```bash
docker compose up -d
```

<details>
<summary>The same with <code>docker run</code></summary>

```bash
docker run -d --name deadfin --restart unless-stopped \
  -p 8096:8096 -p 7359:7359/udp \
  -e TMDB_API_KEY=your-key \
  -v "$PWD/deadfin/data:/var/lib/deadfin" \
  -v /srv/media:/media:ro \
  ghcr.io/deadfin-org/deadfin:alpha
```

</details>

**4. Finish setup.** Open `http://<host>:8096/web/` from your local network
and complete the startup wizard. Behind a reverse proxy, use the one-time
setup link that Deadfin prints in its log. See
[first-run setup](docs/alpha/install.md#first-run-setup-and-the-setup-link).

<details>
<summary>Hardware transcoding: device passthrough</summary>

Pass the GPU through as you would for Jellyfin, then choose the type in
Dashboard → Playback → Transcoding.

**Intel QSV or VAAPI** (iHD driver). The container user needs the host's
`render` group:

```yaml
services:
  deadfin:
    devices:
      - /dev/dri:/dev/dri
    group_add:
      - "${RENDER_GID}"   # getent group render | cut -d: -f3
```

With `docker run`:
`--device /dev/dri:/dev/dri --group-add "$(getent group render | cut -d: -f3)"`.

**NVIDIA NVENC** (Linux, with the NVIDIA Container Toolkit):

```yaml
services:
  deadfin:
    environment:
      NVIDIA_DRIVER_CAPABILITIES: "compute,video,utility"
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: 1
              capabilities: [gpu]
```

With `docker run`: `--gpus all -e NVIDIA_DRIVER_CAPABILITIES=compute,video,utility`.

</details>

### Use jellyfin-web instead

For the complete stock interface with the admin dashboard, put jellyfin-web
12.0 in `./deadfin/web`. Copy it out of the Jellyfin image:

```bash
mkdir -p ./deadfin/web
cid=$(docker create jellyfin/jellyfin@sha256:2d1a69be5aa2695d0c2df90ebeb76bbcc6efc0a798cb09bfa65906680e1d785a)
docker cp "$cid":/jellyfin/jellyfin-web/. ./deadfin/web/
docker rm "$cid"
```

Then mount it at `/var/lib/deadfin/web` and restart Deadfin:

```yaml
    volumes:
      - ./deadfin/data:/var/lib/deadfin
      - ./deadfin/web:/var/lib/deadfin/web:ro
```

When `<data root>/web` holds an `index.html`, Deadfin serves it at `/web/`
in place of the bundled deadfin-web. Remove the mount to go back to the
bundled client.

The full guide, with volumes, reverse proxies, adding libraries and
upgrades, is [docs/alpha/install.md](docs/alpha/install.md).

## Moving from Jellyfin

Deadfin imports from a **copy** of Jellyfin's database and never writes to
Jellyfin's files. Stop Jellyfin, copy `data/jellyfin.db*` and
`root/default`, then run the importer with Deadfin stopped:

```bash
docker compose run --rm -v ./import:/import:ro deadfin \
  import-jellyfin --jellyfin-db /import/jellyfin.db --jellyfin-root /import/root-default --dry-run
```

Drop `--dry-run` to write. Scan, then import once more to match watch state
to the scanned items. The import is safe to run again. Step by step:
[docs/alpha/importing-from-jellyfin.md](docs/alpha/importing-from-jellyfin.md).

## Configuration

Most settings live in the dashboard, as in Jellyfin. The process itself
takes a few flags and environment variables, all optional.

| Setting | Default | Purpose |
|---|---|---|
| `--listen` | `:8096` | TCP address for the HTTP server. |
| `--program-data` | `DEADFIN_DATA_DIR`, then `JELLYFIN_DATA_DIR`, then `./data` | Data root: config, database, metadata, cache, logs. |
| `DEADFIN_DATA_DIR` | unset; the image sets `JELLYFIN_DATA_DIR=/var/lib/deadfin` | Data root when `--program-data` is not set. Deadfin serves `/web` from `<data root>/web` when it holds an `index.html`, and from the bundled deadfin-web otherwise. `JELLYFIN_DATA_DIR` is the accepted fallback when `DEADFIN_DATA_DIR` is not set. Deadfin logs a warning for each set `JELLYFIN_CONFIG_DIR`, `JELLYFIN_CACHE_DIR`, `JELLYFIN_LOG_DIR` or `JELLYFIN_WEB_DIR` and uses `<data root>/config`, `cache`, `log` and `web`. |
| `--database-url`, `DEADFIN_DATABASE_URL` | empty: SQLite in `<data root>/data/deadfin.db` | A `postgres://user:password@host:5432/dbname` URL selects PostgreSQL. The flag wins over the variable. The maintenance commands read the same setting. |
| `TMDB_API_KEY` | unset | TMDb v3 key for movie and TV metadata and artwork. |
| `AUDIODB_API_KEY` | unset | TheAudioDB key for music artwork and bios. |
| `LISTENBRAINZ_TOKEN`, `LISTENBRAINZ_SERVER` | unset | ListenBrainz scrobbling. |
| `DEADFIN_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `DEADFIN_SQLITE_CACHE_MB`, `DEADFIN_SQLITE_MMAP_MB` | SQLite defaults | SQLite page cache and memory map. Raise them for very large libraries. |
| `DEADFIN_PPROF_ADDR` | unset | Go profiler address. Use a loopback address, for example `127.0.0.1:6060`. |

The binary also has a few maintenance commands, for example
`deadfin add-library --name Movies --type movies --path /media/movies` and
`deadfin version`.

## Clients

How each app was tested: **app** means the real app ran against Deadfin;
**QA** means the Apple TV QA matrix (server decisions and checks plus a macOS
AVPlayer probe, for the Swiftfin and SharkParty profiles); **API** means only
the app's own requests (its device profile and URLs) were sent to Deadfin.

| Client | Sign-in | Browse | Direct play | Transcode | Subtitles |
|---|---|---|---|---|---|
| jellyfin-web 12.0 | ✅ app | ✅ app | — | — | — |
| Swiftfin (tvOS 1.4.1, native player) | — | ✅ app | ✅ app; DV profile 7 ⚠️ | ⚠️ | QA |
| Swiftfin (iOS) | ✅ API | ✅ API | ✅ API | ✅ API | ✅ API |
| Infuse 8.5.7 for Mac | ✅ app | ✅ app | ✅ app | direct-plays | — |
| Jellyfin for Android TV | — | — | ✅ API | ✅ API | ✅ API |

— means no result yet. ⚠️: on Apple TV, Swiftfin's native player failed on
Dolby Vision profile 7 and on a 20 Mbit/s H.264 transcode with rc.5. The fix
is in v0.3.0-rc.6; the Apple TV retest is pending. Versions, dates, limits
and the test steps for every other client are in
[docs/alpha/client-matrix.md](docs/alpha/client-matrix.md).
Tested a client? Open a "Client compatibility report" issue.

## Build from source

You need:

- Go 1.26.10 with cgo
- libvips development files and `pkg-config`
- `ffmpeg` and `ffprobe` on `PATH` at run time. For hardware transcoding and
  tone mapping, use
  [jellyfin-ffmpeg](https://github.com/jellyfin/jellyfin-ffmpeg) 8.1.1-1,
  the build the release image ships.

`shell.nix` provides the build dependencies (`nix-shell`).

```bash
go build -o deadfin ./cmd/deadfin
./deadfin --program-data ./data --listen :8096
```

`make build` compiles every package and `make test` runs the tests. On
Linux, the image tests load the SkiaSharp native library from
`/usr/local/lib/deadfin/libSkiaSharp.so`. `make skia` installs it from the
SkiaSharp.NativeAssets.Linux NuGet package, verified by SHA-256, and caches it
for later runs. To build a local container image:

```bash
make image PLATFORM=linux/amd64
```

## Provenance

Deadfin is a source-informed port of Jellyfin, written with AI coding agents
and reviewed by its maintainer. It follows one pinned Jellyfin revision. See
[PROVENANCE.md](PROVENANCE.md).

## License

Deadfin is licensed under the GNU General Public License, version 2 only
(`GPL-2.0-only`). See [LICENSE](LICENSE), and
[PROVENANCE.md](PROVENANCE.md) for why version 2.

<details>
<summary>Third-party software in the container image</summary>

The release image includes third-party programs under their own licenses.
Deadfin runs ffmpeg as a separate process.

- **jellyfin-ffmpeg 8.1.1-1**, the Jellyfin project's build of FFmpeg with
  tonemapx and the Intel QSV and VAAPI patches. It is a GPL-3.0-or-later
  build. Source:
  <https://github.com/jellyfin/jellyfin-ffmpeg/releases/tag/v8.1.1-1>.
- **libvips** (LGPL-2.1-or-later), from Debian bookworm.
- **SkiaSharp 3.119.4** native library (MIT), from the
  SkiaSharp.NativeAssets.Linux NuGet package: `third_party/skia-sharp/`.

`third_party/deadfin-image/NOTICE` (in the image:
`/usr/share/doc/deadfin/NOTICE`) lists every component, where its license
text is, and our written offer of the corresponding source.

</details>

## Support

Ask questions in
[GitHub Discussions](https://github.com/deadfin-org/deadfin/discussions).
Report bugs with an issue form. [SUPPORT.md](SUPPORT.md) says where each
kind of report goes.

## Security

Report security problems privately. See [SECURITY.md](SECURITY.md).

## Contributing

Bug reports, client reports and small, focused pull requests are welcome.
Read [CONTRIBUTING.md](CONTRIBUTING.md) and
[docs/alpha/reporting-bugs.md](docs/alpha/reporting-bugs.md). The
[roadmap](ROADMAP.md) lists the work that comes next.

## Acknowledgements

Deadfin exists because of [Jellyfin](https://jellyfin.org) and its
contributors, who built the server, the API and the clients that Deadfin
follows, and [jellyfin-ffmpeg](https://github.com/jellyfin/jellyfin-ffmpeg),
which does the transcoding. Thank you.

Deadfin is not affiliated with or endorsed by the Jellyfin project. Jellyfin
is a trademark of its respective owners.
