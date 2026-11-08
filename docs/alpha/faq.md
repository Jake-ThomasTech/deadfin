# Frequently asked questions

## My movies and shows have no artwork or metadata

Set a TMDb API key. Deadfin has no built-in TMDb key (D-11), so each server
uses its own. Without one, movies and shows keep their file and folder names
and get no artwork.

1. Get a free v3 key at
   [themoviedb.org/settings/api](https://www.themoviedb.org/settings/api).
2. Set `TMDB_API_KEY` in the container environment and restart Deadfin.
3. Run a metadata refresh on each library that was scanned before the key
   was set. Set the key before the first scan to skip this step.

Music artwork and bios come from TheAudioDB; set `AUDIODB_API_KEY` for them.
See [install.md](install.md#environment-variables).

## An HDR or Dolby Vision file does not transcode

Deadfin offers a transcode of an HDR source only when it can tone-map it to
SDR (D-32). The release image ships Jellyfin's FFmpeg, which tone-maps 10-bit
HDR10, HLG and Dolby Vision in software, so these sources transcode as in
Jellyfin. Intel QSV and VAAPI can tone-map on the GPU when tone mapping is
on in Dashboard → Playback → Transcoding.

Two kinds of source have no tone mapper: 8-bit HDR, and Dolby Vision with an
SDR fallback layer. For these, the server answers that the file cannot
transcode, and the client must direct-play it. A native install with a
generic FFmpeg build has no software tone mapper; use
[jellyfin-ffmpeg](https://github.com/jellyfin/jellyfin-ffmpeg).

## Where is the web client?

The image includes deadfin-web. Deadfin serves it at
`http://<host>:8096/web/`. To use jellyfin-web 12.0 instead, put its files in
`./deadfin/web` and mount that directory at `/var/lib/deadfin/web`. The
commands are in [install.md](install.md#web-client). Every other Jellyfin
client connects with the server URL, as it does to Jellyfin.

## How do I move from Jellyfin?

Run the one-way importer against a copy of Jellyfin's database. It brings
users and passwords, device sign-ins, watch state, playlists, collections
and library definitions. It never writes to Jellyfin's files, so Jellyfin
stays ready as a fallback. Follow
[importing-from-jellyfin.md](importing-from-jellyfin.md).

## SQLite or PostgreSQL?

Use SQLite unless you already run PostgreSQL. SQLite is the default: one
file, `data/deadfin.db`, under the data directory, and no setup. Keep it on
local or block storage, not NFS or SMB.

PostgreSQL (tested on 17 and 18) suits an operator who already runs and backs up a
PostgreSQL server. Set `DEADFIN_DATABASE_URL`. Choose at install time: a
server that changes the setting later starts with an empty database. Both
run one Deadfin process per data directory (D-06). See
[install.md](install.md#database).

## How do I use my GPU for transcoding?

Deadfin has hardware transcoding for Intel QSV and VAAPI (iHD driver) and
NVIDIA NVENC on Linux (D-37). Other types, and VAAPI on AMD, transcode in
software.

- **Intel QSV or VAAPI**: pass `/dev/dri` (or only `/dev/dri/renderD128`)
  into the container and add the host's `render` group.
- **NVIDIA NVENC**: install the NVIDIA Container Toolkit, give the container
  a GPU, and set `NVIDIA_DRIVER_CAPABILITIES=compute,video,utility`. Deadfin
  tests each NVENC encoder at startup and uses only the ones the GPU accepts.
  After a GPU change, restart Deadfin.

Then choose the type in Dashboard → Playback → Transcoding. The compose and
`docker run` lines are in the [README](../../README.md#quick-start) and
[deployment-security.md](deployment-security.md#3-docker-compose-reference).

## The setup wizard returns 403 behind my reverse proxy

Until setup is complete, the wizard opens only for direct requests from the
local network (D-53). Through a proxy or from another network, open the
one-time setup link from the server log:

```
level=WARN msg="first-run setup is pending: ..." path="/deadfin/v1/setup?token=<64 hex digits>"
```

Open `https://<your-public-url>/deadfin/v1/setup?token=<token>` once. The
token changes at each restart while setup is pending. Treat it as a
password. See
[install.md](install.md#first-run-setup-and-the-setup-link).

To see each client's own address behind a proxy, list the proxy in Known
Proxies: Deadfin then reads `X-Forwarded-For` from it. Forward WebSockets on
`/socket`, and do not buffer `/Videos/` and `/Audio/`. See
[install.md](install.md#reverse-proxy).

## Which clients work?

jellyfin-web 12.0, Swiftfin, Jellyfin for Android TV and Infuse are
verified. Results for each client and feature, and the steps to test a
client yourself, are in [client-matrix.md](client-matrix.md). Tested a
client? Open a "Client compatibility report" issue.

## Does Deadfin have Live TV or SyncPlay?

Not yet. Live TV, DVR and SyncPlay menus are hidden or empty (D-07). Both
are planned: see the [roadmap](../../ROADMAP.md). Plugins and DLNA are not
available.

## Where do I ask a question or report a bug?

Questions go to
[GitHub Discussions](https://github.com/deadfin-org/deadfin/discussions).
Bugs go to an issue; read [reporting-bugs.md](reporting-bugs.md) first.
Security problems go through [SECURITY.md](../../SECURITY.md). See
[SUPPORT.md](../../SUPPORT.md).
