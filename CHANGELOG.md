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

- A library scan follows symbolic links, as upstream does. A linked media
  file or a linked directory, also one outside the library root, enters the
  library under the link path; before, the scan left every link out. A
  dangling link is skipped with a warning. A directory link to the directory
  that holds it or to one of its ancestors is skipped with a warning (D-86).
  A file link's snapshot now carries the facts of its target, so playback and
  probing check the target file.
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

[Unreleased]: https://github.com/deadfin-org/deadfin/compare/v0.3.0-rc.3...HEAD
[0.3.0-rc.3]: https://github.com/deadfin-org/deadfin/releases/tag/v0.3.0-rc.3
