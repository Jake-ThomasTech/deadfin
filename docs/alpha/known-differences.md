# Known differences from Jellyfin

Deadfin copies the behavior of Jellyfin 12.0.0. Where it differs on purpose,
the difference has an ID (D-nn) in [../divergences.md](../divergences.md).
This page tells you, in plain words, the differences that you can see as a
user or an administrator. It also lists the gaps that the alpha still has.

Before you report a bug, check this page. If a difference here causes you a
real problem, report it anyway and tell us why.

## Differences on purpose

### Setup and security

- **The first-run wizard is open only on your local network** (D-53). Through
  a reverse proxy or from another network, use the setup link from the
  server log. See [install.md](install.md#first-run-setup-and-the-setup-link).
- **Forwarded client addresses are not trusted.** Deadfin ignores
  `X-Forwarded-For` and the Known Proxies setting. Behind a proxy, every
  client shows the proxy's address.
- **QuickConnect has limits** (D-55). One address can have at most 16 open
  QuickConnect requests, and the server at most 256. Past that, new requests
  fail with "429" until old ones expire (10 minutes). Behind a proxy all
  clients share one address.
- **Remote image downloads refuse local addresses** (D-27). "Download
  image" accepts any http or https URL, but not one on your own network or
  `localhost`, such as a NAS. The error names the reason. Upload the image
  file instead.
- **Media attachments** other than fonts and images download as files
  (D-64), for example an HTML or SVG attachment in an MKV file.

### Metadata and artwork

- **No built-in TMDb key** (D-11). Jellyfin ships a shared key. Deadfin does
  not. Without `TMDB_API_KEY`, movies and shows get no remote metadata or
  artwork; they keep the names from their files and folders, years included.
- **Only built-in providers** (D-08, D-28). There are no plugins and no
  plugin catalog. Provider ID lists show only the providers Deadfin has.
- **Uploaded images stay in the metadata folder** (D-44). They get a content
  tag in the file name and are not saved next to your media as `poster.jpg`.
  Image tags in the API are file names, not hashes (D-12).
- **Edits are not written to NFO files** (D-39). Deadfin stores your edits in
  its database only.
- **Searching anime finds alternative titles** (D-48): English, romaji and
  native titles from AniList.
- **Rescans keep remote data** (D-40, D-34). A rescan or a manual refresh does
  not drop provider IDs and images that the scan cannot find again.

### Libraries and browsing

- **Library type values are strict** (D-26). An unknown or comma-separated
  `CollectionType` is not saved.
- **Mixed libraries** (no content type) treat a folder with one video and no
  subfolder as a movie (D-56). A movie folder with an `Extras` folder stays a
  folder in a mixed library. Use a Movies library for movies with extras.
- **Season counts** (D-57) always include specials that air within the
  season, also when you turn off "Display specials within seasons".
- **Virtual seasons** (D-51) are made only for episodes outside a season
  folder, and get no season artwork or season metadata.
- **Playlists and collections that you make in Deadfin** are kept in the
  database only (D-42, D-43). Jellyfin will not see them if you go back.

### Playback

- **Audio and subtitle choice carries to the next episode** (D-36). If you
  choose the Japanese track and English subtitles on episode 1, episode 2
  starts with the same choices. Jellyfin remembers the choice per item only.
- **Hardware transcoding** (D-37) works for Intel QSV/VAAPI (iHD driver) and
  NVIDIA NVENC on Linux. Other types (AMD AMF, VideoToolbox, Rockchip,
  V4L2) and AMD VAAPI transcode in software.
- **HDR without tone mapping** (D-32). If the server cannot tone-map a source
  (8-bit HDR, Dolby Vision with SDR fallback), it does not offer a transcode.
  The client must direct-play it. 10-bit HDR10, HLG and Dolby Vision
  transcode with tone mapping, as in Jellyfin.
- **Audio in HLS** (D-50). When a stream cannot copy the source audio, Deadfin
  converts it to AAC.
- **PGS subtitles** (D-52) can be fetched as `.sup` as well as `.pgssub`, so
  clients that ask for `.sup` work.
- **Subtitle uploads** (D-47) go to the metadata folder when your media
  folder is read-only.

### Not in Deadfin

- **Live TV and DVR, SyncPlay** (D-07): the menus are hidden or empty.
- **Plugins** (D-08): there is no plugin catalog and no installs.
- **DLNA**: not available.
- **Several servers sharing one database** (D-06): not supported. Run one
  Deadfin per data directory.

## Gaps in this alpha

These are defects or missing parts. Fixes may land during the alpha.

- Dashboard pages for devices, logs and NFO settings show errors.

