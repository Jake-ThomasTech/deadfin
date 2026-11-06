# Changelog

All notable changes to Deadfin are recorded in this file. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and Deadfin uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The release workflow copies the section of a tag into its GitHub Release. A
`vX.Y.Z` tag needs a `## [X.Y.Z]` section. A `vX.Y.Z-rc.N` tag uses its own
section, then the `X.Y.Z` section, then `Unreleased`.

Container images: `ghcr.io/deadfin-org/deadfin:<tag>`. `:alpha` follows the
newest release or release candidate. `:latest` follows the newest release.

## [Unreleased]

## [0.4.0-rc.1] - 2026-11-06

First release candidate for v0.4.0. It adds Live TV, transcode workers,
restart continuity, the stream edge and server replicas. The image bundles
deadfin-web v0.4.0-rc.1 at `/web/`, with the transcode workers page and
Live TV for viewers.

### Added

- Live TV (D20). An M3U tuner reads the channel playlist, and an XMLTV
  listings provider fills the guide. The Refresh Guide task stores channels
  and programmes as items, so the Jellyfin Live TV API, item queries and
  search hints return them. Channels play through a shared stream broker:
  viewers of one channel share one upstream connection, an HLS source is
  read once through an ffmpeg remux, a dropped or stalled upstream
  reconnects, and an expired channel URL is renewed with one playlist
  re-read (D-87, D-88, D-93, D-103). An M3U tuner filters channels by
  `group-title` (`IncludeGroups`, `ExcludeGroups`) and stores the group as
  channel tags (D-90, D-91). The guide reads `SxxEyy` episode numbers and
  the "New" marker from IPTV descriptions (D-123). Timer and series-timer
  requests return 400 (D-122).
- Live TV channel lists. Playlists and collections accept channels, so an
  administrator can share curated channel lists (D-94). The user policy
  field `LimitLiveTvToSharedLists` limits a user to the channels in lists
  they can see (D-105). `EnableLiveTvAccess` hides every Live TV item from
  a user without access (D-95).
- Remote transcode workers (D-118). `deadfin worker` runs HLS and
  progressive transcodes on another host, with its own GPU (QSV, VA-API or
  NVENC). A worker dials out to the server and authenticates with a static
  token (`DEADFIN_TRANSCODE_WORKER_TOKENS`) or an enrolled identity, over
  TLS. A placement policy (`local`, `local-first`, `workers-first`,
  `workers-only`) with pools and rules decides where each job runs, with a
  local GPU and CPU fallback. Text subtitle burn-in runs on workers. A
  worker that stops or disconnects hands its jobs to another host at the
  next segment. Container image, packages, a Helm chart, KEDA scaling
  signals and a scale hook are included. Plain HTTP for workers on a
  trusted network is a dashboard setting or
  `DEADFIN_TRANSCODE_WORKERS_ALLOW_HTTP`. See
  [docs/transcode-workers.md](docs/transcode-workers.md).
- Transcode workers: placement prefers the worker that decodes and encodes
  the job in hardware, and in a mode other than `local` the HLS output
  codec can be one that only a worker encodes, such as AV1 (D-125).
- Playback continues through a server restart. With
  `DEADFIN_PLAY_SESSION_RESTORE=true`, server replicas or the stream edge,
  play sessions and their `PlaySessionId` grants persist across a restart.
  A worker keeps its
  jobs for the job lease (`DEADFIN_TRANSCODE_JOB_LEASE`, 60 s) so the next
  server process adopts them.
- Stream edge (D-119, D-120). `deadfin edge` serves HLS segments that workers
  mirror to it, and direct-play streams from the library, while the server
  restarts. It needs `DEADFIN_EDGE_KEY` on the server.
- Server replicas on PostgreSQL (D-121). With `DEADFIN_REPLICA_URL`, two or
  more server processes share one database. Each play session and transcode
  job has one owner replica, other replicas forward its requests there, and
  a replica that stops hands its sessions and jobs to a peer, so a rolling
  update keeps playback. `GET /Sessions`, the dashboard's session list, remote-control
  commands and WebSocket events (user data, library changes, refresh
  progress, activity) span all replicas.

### Changed

- `GET /Search/Hints` is faster on large libraries. It matches anime titles
  once per request, and an index covers the search scan.
- Filtered and visible item pages read straight from the items table.

### Fixed

- An HLS seek no longer hangs when the restarted transcode takes more than
  about 5 s to write its first segment, for example Intel QSV HEVC output
  (#858). Before the first segment download, the throttler measures the
  lead from the seek position, not from the start of the file, so it no
  longer pauses ffmpeg before the requested segment is complete. 0.3.1 has
  the same defect.
- A progressive video transcode (`/Videos/{id}/stream.{mkv,ts,mp4}`), which
  PlaybackInfo offers to a client with a `Protocol: http` transcoding
  profile, now plays (#857). It encodes H.264 (libx264) and AAC when the
  streams cannot be copied (D-126). Before, the route refused the URL with
  400 or 501. 0.3.1 has the same defect.
- An HLS transcode that burns in an embedded SubRip (or other non-ASS
  text) subtitle reads the cached subtitle extraction, as Jellyfin does
  (#860). Before, libass read the whole media file at every start and
  seek: about 160 s per seek on a 32 GB remux. The first burn-in of a file
  still extracts its subtitles once. 0.3.1 has the same defect.
- Behind a reverse proxy listed in `KnownProxies`, the client address comes
  from `X-Forwarded-For`, as in Jellyfin (#864, GitHub #24). Sessions, the
  activity log, the local-network bitrate rules and the QuickConnect
  per-address limit (D-55) see each client instead of the proxy. With
  `KnownProxies` empty nothing changes. The first-run wizard still refuses
  proxied requests without the setup link (D-53).
- A box set's TMDb ID is always a TMDb collection ID (#846, GitHub #18). A
  box set that has a member movie's TMDb ID gets its members'
  `TmdbCollection` ID on the next library scan or metadata refresh, or
  searches TMDb by name when they have none, and then gets its poster.
  `import-jellyfin` replaces a different stored TMDb ID with the Jellyfin
  collection ID (`collection.xml` `<TmdbId>`) (D-116).
- A metadata refresh fills a missing image from the enabled remote image
  fetchers, as upstream does (#847, GitHub #19). An album without a cover
  gets the TheAudioDB cover on a single-item refresh with
  `ImageRefreshMode=FullRefresh` and in the *Refresh Metadata* task when the
  library's `ImageFetchers` enable TheAudioDB, also when its
  `MetadataFetchers` do not. Before, only the metadata providers set images
  (D-124).
- HLS: a source that the probe could not size (an IPTV stream joined mid-GOP)
  transcodes, and a video copy of it uses MPEG-TS segments, not fMP4
  (D-106).
- `GET /Playlists/{playlistId}/Items` honours `userId`.

## [0.3.1] - 2026-11-02

### Security

- The toolchain is Go 1.26.10. It fixes GO-2026-6809 in `crypto/x509`,
  which the HTTPS server reaches through certificate verification.

## [0.3.0] - 2026-11-02

v0.3.0 is the first public alpha of Deadfin.

### Highlights

- **The image includes deadfin-web.** Deadfin serves the bundled web client
  (v0.3.0-rc.4) at `/web/` with no extra download. To use jellyfin-web
  instead, put its files in `./deadfin/web` and mount that directory at
  `/var/lib/deadfin/web`; a `<data root>/web` with an `index.html` replaces
  the bundled client. An existing `./deadfin/web` mount keeps working and
  keeps its own files: remove the mount to use the bundled deadfin-web.
- **Audio and subtitle choice carries to the next episode** (D-36). When you
  pick a language or a subtitle track in one episode, the next episode of the
  same series starts with the matching tracks. Subtitles off also carries.
- **Anime search finds every title** (D-48). Search matches the English,
  romaji, native and alias titles from AniList and AniDB, with a kana/romaji
  fold. "Shingeki" and "進撃" both find "Attack on Titan".
- **PostgreSQL support.** SQLite stays the default. Set
  `DEADFIN_DATABASE_URL` (or `--database-url`) to a `postgres://` URL to run
  on PostgreSQL. See [docs/alpha/install.md](docs/alpha/install.md#database).
- **Security fixes** from the release-candidate review: sign-out revokes the
  token, video attachments are scoped to the user, the image downloader
  refuses special-purpose address ranges, and the toolchain is Go 1.26.9.

### Added

- `CHANGELOG.md`. Each GitHub Release body comes from its section here.
- Release images carry a build provenance attestation and an SPDX SBOM for
  each architecture. The SBOMs are attached to the GitHub Release.
- The `:alpha` image tag. It follows the newest release or release
  candidate. The `:latest` tag follows the newest release (no `-rc.N`), so
  v0.3.0 publishes it first.
- Person pages show a biography, birth date, death date and birthplace from
  TMDb. Opening a person without metadata fetches it, and the "Refresh
  People" scheduled task fetches it for every person not yet refreshed
  (D-74). Cast and crew keep their TMDb person ID. This needs a TMDb API
  key (D-11).
- The integrations guide: connect Seerr, Sonarr, Radarr and Bazarr beside
  Jellyfin and after the cutover, with a script that moves Seerr users to
  their Deadfin IDs. See
  [docs/alpha/integrations.md](docs/alpha/integrations.md).

### Changed

- The startup log names the web client in use: its directory, the source
  (`data root` or `bundled`) and the version from its `build.json`.
- The quick start and the install guide put TMDb API key setup first.
- `DEADFIN_DATA_DIR` sets the data root. `JELLYFIN_DATA_DIR` stays as the
  fallback. The image sets its default, `/var/lib/deadfin`, in `JELLYFIN_DATA_DIR`,
  so a deployment that sets either variable overrides it. Deadfin
  logs a warning for each set `JELLYFIN_CONFIG_DIR`, `JELLYFIN_CACHE_DIR`,
  `JELLYFIN_LOG_DIR` or `JELLYFIN_WEB_DIR`.

### Security

- `POST /Sessions/Capabilities` and `POST /Sessions/Capabilities/Full`
  refuse another user's session ID from a non-administrator caller (D-67).
- `/web/` responses send `Referrer-Policy: no-referrer` and
  `X-Content-Type-Options: nosniff`. When the served client is deadfin-web,
  they also send a restrictive `Content-Security-Policy` with
  `frame-ancestors 'none'` (D-68).
- The one-time setup cookie is `Secure` when a `KnownProxies` peer reports
  HTTPS in `X-Forwarded-Proto`. Behind a TLS-terminating proxy, list the
  proxy in `KnownProxies` before first start (D-53).

### Fixed

- A MusicBrainz timeout or rate-limit wait no longer fails the whole artist
  folder. When a remote metadata provider's own request deadline expires, the
  item keeps its stored metadata, the scan logs one `remote metadata provider
  failed` warning, and the folder stays `ok`. A cancelled scan still stops
  (D-107, #749; regression of #367).
- A music album carries the artists of its songs, as upstream does:
  `AlbumArtist`, `AlbumArtists`, `Artists` and `ArtistItems`. The album
  artists of most songs come first. Before, no album had an album artist, so
  clients showed none on album tiles (#739). An album scanned before this
  change gets its artists on the next library scan (D-110). A locked album
  keeps its values.
- *Refresh Metadata* saves provider IDs and images for every library root
  again. A cast member, studio, or genre whose name differs only in case from
  the stored one (`Starz` and `STARZ`) has the same item ID but a different
  path, and the refresh failed the whole folder with `path mismatch`; for a
  library root, nothing was saved. The refresh now keeps the stored person,
  studio, or genre path, as upstream does. A stale row of a removed
  case-variant folder no longer fails its surviving sibling with
  `parent mismatch`: the refresh moves it under the scanned folder, as an
  ordinary scan does (#748).
- A music track whose file carries an embedded cover (an attached picture,
  as FLAC and MP3 files often do) gets that cover as its Primary image, and
  an album without a cover file takes the Primary image of its first track.
  Before, the embedded cover was never extracted, so an album without a
  `folder.jpg` had no image (#784, D-108).
- A library scan gives a music track that has no Primary image its embedded
  cover, also when the file did not change. Before, a track scanned before
  #784 got its cover only on a full image refresh, so the album stayed
  without a cover after an upgrade (#804, D-117).
- A refresh with `ReplaceAllImages` keeps a stored image that no provider
  replaces, as upstream does. Before, it removed every stored image, so an
  album with a remote cover and no other source lost its cover (#804).
- `GET /Items/{itemId}/RemoteImages` lists TheAudioDB for a music album
  (Primary, Disc) by its MusicBrainz release-group ID, and for a music
  artist (Primary, Logo, Banner, Backdrop) by its MusicBrainz artist ID, as
  upstream `AudioDbAlbumImageProvider` and `AudioDbArtistImageProvider` do.
  Before, no remote image provider supported music items, so a cover could
  not be picked in the image editor. TheAudioDB needs `AUDIODB_API_KEY`
  (D-11) (#807).
- A library scan follows symbolic links, as upstream does. A linked media
  file or a linked directory, also one outside the library root, enters the
  library under the link path; before, the scan left every link out. A
  dangling link is skipped with a warning. A directory link to the directory
  that holds it or to one of its ancestors is skipped with a warning (D-86).
  A file link's snapshot now carries the facts of its target, so playback and
  probing check the target file.
- Every artist named in an `ARTIST` or `album_artist` tag gets a MusicArtist
  item after a library scan, as upstream `ArtistsValidator` does. Before,
  only artist folders made artists, so `/Artists` and `/Artists/AlbumArtists`
  left out featured artists and album artists without a folder (#739). An
  artist folder serves its own name. An artist that no track names any more
  is deleted on the next scan, unless it is locked.
- The library scan logs a folder failure when the folder fails. Before, the
  `scanner folder` lines came only after the whole scan ended, so a scan that
  stalled logged no failure at all (#702).
- A library scan keeps a folder that holds two names that differ only in
  case, such as `Live` and `live` on a case-sensitive NFS or ZFS share. Both
  names gave the same item ID, and the whole folder failed on every scan: an
  artist's discography, a season, or at the library root the whole library
  stayed missing. The scan now keeps the first name in ordinal order and logs
  the other as skipped (D-85). A directory entry whose file status cannot be
  read is skipped with a warning; before, one such entry failed the listing
  of its whole directory (#702).
- A multi-disc album is one album, as in Jellyfin. An album folder whose
  tracks are in disc folders (`CD 01`, `Disc 1`, `Digital Media 01`, and the
  other Jellyfin disc names) is the MusicAlbum, its parent folder is the
  artist, and the disc folders are plain folders. The album lists the tracks
  of every disc, its track count includes every disc, and each track shows
  the album. Before, each disc folder was an album named after the folder,
  with no album artist. A library scan replaces those albums (#703).
- The library scan ignores Jellyfin trickplay directories, as Jellyfin does.
  With "save trickplay with media" on, Jellyfin writes a
  `<video>.trickplay/` directory next to each video. Deadfin indexed these
  directories and their tile directories as Folder items that showed in
  browse and search. The next scan removes Folder items that an earlier scan
  made for them (#704).
- The library scan ignores Synology and QNAP system directories, as
  Jellyfin does: `@eaDir`, `eaDir`, `#recycle`, `@Recycle`, `.@__thumb`,
  `$RECYCLE.BIN`, `System Volume Information` and `.grab`. On a Synology
  share, Deadfin indexed the `@eaDir` thumbnail directories as items, and an
  `@eaDir` at the root of a Shows library became a series named "@eaDir".
- HLS copies a Dolby Vision profile 7 (dual-layer) HEVC stream as HDR10 for a
  client that lists `HDR10` but not `DOVIWithEL` in its range types. The copy
  removes the RPU and the enhancement layer with `hevc_metadata=remove_dovi`
  (or `dovi_rpu=strip`) and the master playlist offers an `hvc1` variant with
  `VIDEO-RANGE=PQ`, as upstream does. Before, such a client got a 4K SDR
  H.264 tone-mapped transcode. Startup probes which removal filters ffmpeg
  has. The same port strips HDR10+ from a profile 8.1 HDR10+ copy for a
  Dolby Vision client, and Dolby Vision from an invalid configuration (#586,
  D-84).
- `import-jellyfin` reads a Jellyfin database in WAL mode from the read-only
  `/import` mount that the import guide uses. The importer opens the stopped
  copy immutable, so SQLite creates no `jellyfin.db-shm` next to it. A
  non-empty `jellyfin.db-wal` stops the import with a message to stop
  Jellyfin and copy again (#697).
- HLS plays an audio track that starts late. A track whose first packet is
  more than 1 s into the file is encoded with leading silence instead of
  copied: AAC when the client accepts it, else AC3 or E-AC3. A copied late
  AC3 track gave an init segment without `dac3` and first segments without
  audio, and AVPlayer failed the item at once. The Italian stereo track of a
  4K remux starts at 33.55 s (#681, D-82).
- Embedded text subtitles of large files load. One ffmpeg pass extracts
  every embedded subtitle stream of a file into the subtitle cache, text
  streams included, as upstream does. The first request waits for the pass
  under `SubtitleExtractionTimeoutMinutes`; later requests read the cache. The
  forced track of a 4K remux returned 500 after 30 s (#676, D-52).
- `GET /UserItems/Resume` and `GET /Users/{userId}/Items/Resume` honor
  `parentId`: they return only the in-progress items under the series,
  season, folder, library, collection, or playlist. Before, they returned the
  whole resume list, so the series Play button in Swiftfin played an
  unrelated movie. `excludeActiveSessions` now drops the items the user is
  playing, and the Latest exclusions apply only to the unscoped list, as
  upstream does (#674).
- Sessions report the real play method. A report with no `PlayMethod` is
  `Transcode` while a transcoding job runs for its `PlaySessionId`, else
  `DirectPlay`, as upstream does. `/Sessions` carries `TranscodingInfo` for a
  running HLS job, so an HLS remux no longer shows as direct play (#635).
- `GET /Library/MediaFolders` returns the library roots, as upstream does:
  the enabled collection folders and the hidden playlists folder, ordered by
  sort name, with the `isHidden` filter, for administrators only. Seerr and
  other Jellyfin tools that list libraries this way work without a fallback.
  The playlists folder projects as `ManualPlaylistsFolder` with
  `CollectionType` `playlists` (#596).
- Comma-delimited enum query parameters drop unknown values, as upstream
  does. Infuse home rows (`/Items/Latest` with an Emby field) load again.
- `GET /MediaSegments/{itemId}` returns an empty `QueryResult` for a known
  item (Infuse called it on every play).
- HLS stream copy of Dolby Vision profile 7 (`DOVIWithEL`) is tagged `hvc1`,
  so the init segment matches the `hvc1` master playlist and AVPlayer can
  decode the HDR10 base layer (D-65).
- The H.264 level in the HLS master playlist matches the level the encoder
  writes (D-66).
- The HLS throttler and the transcode cache limits pause ffmpeg again.
  jellyfin-ffmpeg 8 does not pause on the old key, so a fast remux kept writing
  segments far ahead of playback.
- A user with access to only some libraries can open a person, genre or
  studio by its ID, as upstream does. Cast portraits returned 404 for these
  users. Lists keep the library restriction.
- Remote images for a movie or series list TMDb's backdrops without text as
  `Backdrop`, as Jellyfin does. The TMDb request carried the metadata
  language, so TMDb returned only the backdrops with text, and Deadfin listed
  them all as `Thumb`. A refresh that replaces all images keeps TMDb's first
  backdrop, not its last (#783).

## [0.3.0-rc.3] - 2026-10-14

Third release candidate for v0.3.0 and the first public cut. The server code
is the same as 0.3.0-rc.2. This tag is the root of the public history at
github.com/deadfin-org/deadfin.

### Added

- Release images on `ghcr.io/deadfin-org/deadfin` for linux/amd64 and
  linux/arm64, built by GitHub Actions, with a GitHub Release for each tag.

## [0.3.0-rc.2] - 2026-10-14

Second release candidate for v0.3.0. It adds the fixes from the
release-candidate security review.

### Security

- `POST /Sessions/Logout` revokes the caller's access token and ends the
  device sessions of that token.
- Video attachments (fonts and other embedded files) are scoped to the user
  of the token. Active content types download as attachments and do not
  render in the browser.
- The remote-image downloader refuses CGNAT (100.64.0.0/10) and the other
  IANA special-purpose address ranges.
- The toolchain is Go 1.26.9, with updated `golang.org/x/image`,
  `golang.org/x/net`, `golang.org/x/text` and `go-chi/chi`. govulncheck
  reports no findings on the binary.

### Added

- The deployment and security guide:
  [docs/alpha/deployment-security.md](docs/alpha/deployment-security.md).

## [0.3.0-rc.1] - 2026-10-14

First release candidate for v0.3.0, the community alpha.

### Added

- PostgreSQL as a database backend, selected with `DEADFIN_DATABASE_URL`.
  SQLite stays the default.
- Release-candidate tags `vX.Y.Z-rc.N` publish images on the `rc` channel.
- The public README, SECURITY, CONTRIBUTING, and issue templates.
- The GPL-2.0 license, PROVENANCE.md, REUSE metadata and SPDX headers. The
  image carries the license notices and a source offer.

### Changed

- The Go module path is `github.com/deadfin-org/deadfin`.

### Fixed

- The SQLite query for folder played counts uses a faster plan on large
  libraries.

[Unreleased]: https://github.com/deadfin-org/deadfin/compare/v0.4.0-rc.1...HEAD
[0.4.0-rc.1]: https://github.com/deadfin-org/deadfin/compare/v0.3.1...v0.4.0-rc.1
[0.3.1]: https://github.com/deadfin-org/deadfin/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/deadfin-org/deadfin/releases/tag/v0.3.0
[0.3.0-rc.3]: https://github.com/deadfin-org/deadfin/releases/tag/v0.3.0-rc.3
