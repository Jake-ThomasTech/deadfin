# Jellyfin Live TV architecture (upstream pin b04614e)

All paths are relative to `upstream/jellyfin`. Line numbers refer to the pinned
tree. "LTV" = `src/Jellyfin.LiveTv`.

---

## 1. Project layout

### 1.1 Assemblies and folders

| Area | Path | Notes |
|---|---|---|
| Implementation | `src/Jellyfin.LiveTv/` | All Live TV logic. NuGet refs: `Jellyfin.XmlTv` (10.12.0-pre1, `Directory.Packages.props`), `AsyncKeyedLock`. |
| DI wiring | `LTV/Extensions/LiveTvServiceCollectionExtensions.cs:25-46` | Singletons: `LiveTvDtoService`, `TimerManager`, `SeriesTimerManager`, `RecordingsMetadataManager`, `ILiveTvManager→LiveTvManager`, `IChannelManager→ChannelManager`, `IStreamHelper→StreamHelper`, `ITunerHostManager`, `IListingsManager`, `IGuideManager`, `IRecordingsManager`, `ILiveTvService→DefaultLiveTvService`, `ITunerHost→HdHomerunHost`, `ITunerHost→M3UTunerHost`, `SchedulesDirect` (as `IListingsProvider` + `ISchedulesDirectService`), `IListingsProvider→XmlTvListingsProvider`. |
| Hosted services | `Jellyfin.Server/Startup.cs:131,152,157` | `AddLiveTvServices()`, `RecordingsHost` (restarts timers, creates recording folders), `RecordingNotifier` (WebSocket timer events). |
| Media source hook | `LTV/LiveTvMediaSourceProvider.cs` | `IMediaSourceProvider`, discovered by assembly scan and fed to `MediaSourceManager.AddParts` (`Emby.Server.Implementations/Library/MediaSourceManager.cs:97`). |
| Config factory | `LTV/Configuration/LiveTvConfigurationFactory.cs:20` | Named config key `"livetv"` → `config/livetv.xml`, type `LiveTvOptions`. |
| `LTV/Channels/` | `ChannelManager`, `RefreshChannelsScheduledTask`, ... | This is the *plugin Channels* feature (`IChannelManager`), not tuner Live TV. It lives in this assembly only for historical reasons. |

LTV sub-folders: `Guide/` (GuideManager, RefreshGuideScheduledTask), `Listings/`
(ListingsManager, XmlTvListingsProvider, SchedulesDirect + DTOs, EpgChannelData,
XmlTvProgramEtag), `TunerHosts/` (BaseTunerHost, LiveStream, SharedHttpStream,
M3UTunerHost, M3uParser, TunerHostManager, `HdHomerun/*`), `Recordings/`
(RecordingsManager, RecordingHelper, RecordingsMetadataManager, RecordingsHost,
RecordingNotifier), `Timers/` (ItemDataProvider, TimerManager,
SeriesTimerManager), `IO/` (IRecorder, DirectRecorder, EncodedRecorder,
ExclusiveLiveStream, StreamHelper), plus top-level `DefaultLiveTvService.cs`,
`LiveTvManager.cs`, `LiveTvDtoService.cs`, `LiveTvMediaSourceProvider.cs`,
`LiveTvChannelImageHelper.cs`.

### 1.2 Interfaces (`MediaBrowser.Controller/LiveTv/`)

`ILiveTvManager`, `ILiveTvService` (also `ISupportsDirectStreamProvider`,
`ISupportsNewTimerIds` in the same file), `ITunerHost` (also
`IConfigurableTunerHost`), `ITunerHostManager`, `IListingsProvider`,
`IListingsManager`, `IGuideManager`, `IRecordingsManager`,
`ISchedulesDirectService`; entities `LiveTvChannel`, `LiveTvProgram`; models
`ChannelInfo`, `ProgramInfo`, `TimerInfo`, `SeriesTimerInfo`,
`ActiveRecordingInfo`, `TimerEventInfo`, `LiveTvConflictException`.

Stream plumbing: `MediaBrowser.Controller/Library/ILiveStream.cs`
(`ConsumerCount`, `OriginalStreamId`, `TunerHostId`, `EnableStreamSharing`,
`MediaSource`, `UniqueId`, `Open`, `Close`, `GetStream`),
`MediaBrowser.Controller/Library/IDirectStreamProvider.cs`.

Models (`MediaBrowser.Model/LiveTv/`): `LiveTvOptions`, `TunerHostInfo`,
`ListingsProviderInfo`, `TimerInfoDto`, `SeriesTimerInfoDto` (also `KeepUntil`
enum), `BaseTimerInfoDto`, `RecordingStatus`, `ChannelType`, `DayPattern`,
`GuideInfo`, `LiveTvInfo`, `LiveTvServiceInfo`, `ChannelMappingOptionsDto`,
`TunerChannelMapping`, queries (`LiveTvChannelQuery`, `RecordingQuery`,
`TimerQuery`, `SeriesTimerQuery`).

### 1.3 Controller

`Jellyfin.Api/Controllers/LiveTvController.cs` (1162 lines). Route prefix
`/LiveTv` from `[Route("[controller]")]` on `BaseJellyfinApiController`
(`Jellyfin.Api/BaseJellyfinApiController.cs:13`). There is **no class-level
`[Authorize]`**; each action has its own policy. Live stream open/close is in
`Jellyfin.Api/Controllers/MediaInfoController.cs` (class-level `[Authorize]`,
line 29).

### 1.4 Service layering (one service only)

`LiveTvManager` is a facade over `ILiveTvService[]`. In-tree there is exactly one
service: `DefaultLiveTvService` (`Name = "Emby"`, `DefaultLiveTvService.cs:32`),
the old "EmbyTV". It owns the tuner hosts (via `ITunerHostManager`), timers and
recordings. External plugins could add more `ILiveTvService`s; the code keeps
multi-service branches (`service is not DefaultLiveTvService`) but nothing in
tree uses them.

ID scheme (`LiveTvDtoService.cs:28-30,403-428`): `InternalVersionNumber = "4"`,
`ServiceName = "Emby"`.
- Channel item id = `LibraryManager.GetNewItemId(lower(serviceName + externalId + "4"), typeof(LiveTvChannel))`.
- Program item id = `GetNewItemId(lower("Emby" + externalId + "4"), typeof(LiveTvProgram))`.
- Timer id (API) = `md5(lower("Emby" + id + "4"))` "N" string; series timer id = same md5 as Guid.

---

## 2. Tuner hosts and live streams

### 2.1 `TunerHostInfo` (stored in `livetv.xml` → `TunerHosts`)

`MediaBrowser.Model/LiveTv/TunerHostInfo.cs`. Fields and constructor defaults
(lines 8-48): `Id`, `Url`, `Type` (`"hdhomerun"`/`"m3u"`), `DeviceId`,
`FriendlyName`, `ImportFavoritesOnly`, `AllowHWTranscoding` (**true**),
`AllowFmp4TranscodingContainer` (false), `AllowStreamSharing` (**true**),
`FallbackMaxStreamingBitrate` (**30000000**), `EnableStreamLooping`, `Source`
(unused in tree), `TunerCount` (0 = unlimited), `UserAgent`, `IgnoreDts`
(**true**), `ReadAtNativeFramerate` (false).

### 2.2 `TunerHostManager` (`LTV/TunerHosts/TunerHostManager.cs`)

- `SaveTunerHost` (62-101): deep-clones via JSON, calls
  `IConfigurableTunerHost.Validate`, assigns `Guid.NewGuid("N")` id if new,
  saves `livetv` config, then `CancelIfRunningAndQueue<RefreshGuideScheduledTask>`.
- `DeleteTunerHost` (104-128): removes host, deletes `cache/<id>_channels`, queues guide refresh.
- `DiscoverTuners(newDevicesOnly)` (131-149): runs each host's `DiscoverDevices`
  for `TunerDiscoveryDurationMs = 3000` (line 24); filters configured `DeviceId`s.
- `ScanForTunerDeviceChanges` (152-180): run at guide refresh. If a discovered
  device's `DeviceId` matches a configured host with a different URL, it updates
  the URL and calls `SaveTunerHost` (which re-queues the guide task).

### 2.3 `BaseTunerHost` (`LTV/TunerHosts/BaseTunerHost.cs`)

- In-memory channel cache per tuner id (26, 50-68); disk fallback
  `cache/<hostId>_channels` JSON, written when `enableCache == false` and read
  when the live fetch fails (77-135). Channels from multiple hosts of the same
  type are de-duplicated by `ChannelInfo.Id` (90).
- `GetChannelStream(channelId, streamId, currentLiveStreams)` (171-223): builds
  the list of all hosts that carry the channel id, then tries each:
  `GetChannelStream(host,…)` + `liveStream.Open()`. Any exception logs and moves
  to the next host. If all fail: `LiveTvConflictException("Unable to find host to play channel")`.
  This is the only failover; it works only when channel ids collide across hosts
  (true for HDHR, which uses `hdhr_<GuideNumber>`; false for M3U, whose ids embed
  `md5(tuner url)`).
- `IsValidChannelId` = prefix check `Type + "_"` (46, 225-230).

### 2.4 HDHomeRun (`LTV/TunerHosts/HdHomerun/`)

`HdHomerunHost.cs`: `Name = "HD Homerun"`, `Type = "hdhomerun"`, channel id
prefix `hdhr_` → `hdhr_<GuideNumber>` (64-71).

- **Discovery** (481-533): UDP broadcast to `255.255.255.255:65001` with fixed
  20-byte packet `{0,2,0,12,1,4,255,255,255,255,2,4,255,255,255,255,115,204,125,143}`
  (494-500). Replies with `ReceivedBytes > 13 && buf[1] == 3` are accepted; for
  each it calls `http://<ip>/discover.json` (`TryGetTunerHostInfo`, 535-550) to
  fill `DeviceId`, `FriendlyName`, `TunerCount`.
- **`/discover.json`** (`GetModelInfo`, 107-163): cached per tuner id in
  `_modelCache`. HTTP 404 → stub `ModelNumber = "HDHR"` (HDHR4 has no such API).
  `DiscoverResponse.cs`: `FriendlyName, ModelNumber, FirmwareName,
  FirmwareVersion, DeviceID, DeviceAuth, BaseURL, LineupURL, TunerCount`;
  `SupportsTranscoding` = `ModelNumber` contains `"hdtc"` (27-38).
- **`/lineup.json`** (`GetLineup`, 73-85): `LineupURL ?? BaseURL + "/lineup.json"`.
  Filters `Favorite` when `ImportFavoritesOnly`; always drops `DRM` channels.
  `Channels.cs` fields: `GuideNumber, GuideName, VideoCodec, AudioCodec, URL,
  Favorite, DRM, HD`. `IsLegacyTuner` = URL starts with `hdhomerun` (102).
- **Media sources** (`GetChannelStreamMediaSources`, 345-383): legacy → one
  `native` source. Otherwise, if model supports transcoding **and**
  `AllowHWTranscoding`: `heavy, internet540, internet480, internet360,
  internet240, mobile` then `native`; else just `native`. (`internet720` is
  understood by `GetMediaSource` but never offered.)
- **`GetMediaSource(profile)`** (187-343): per-profile dimensions/bitrate table
  (`mobile` 1280x720 2 Mb; `heavy` 1920x1080 15 Mb; `internet720` 8 Mb;
  `internet540` 960x540 2.5 Mb; `internet480` 848x480 2 Mb; `internet360` 1.5 Mb;
  `internet240` 1 Mb). Native: interlaced=true, 1920x1080 dummy if HD (comment:
  "android tv's 1200 condition"). Defaults: video 15 Mb HD / 2 Mb SD, audio
  448k / 192k. `mpeg2`→`mpeg2video`; h264 sets `NalLengthSize="0"`. Id =
  `<profile|native>_<md5(channelId)>_<md5(url)>`. Flags: `Protocol=Udp`,
  `Container="ts"`, `RequiresOpening=true`, `RequiresClosing=true`,
  `BufferMs=0`, `SupportsDirectPlay=false`, `SupportsDirectStream=true`,
  `SupportsTranscoding=true`, `IsInfiniteStream=true`, `IgnoreDts=true`,
  `UseMostCompatibleTranscodingProfile=true` ("All HDHR tuners require this"),
  `FallbackMaxStreamingBitrate=info.FallbackMaxStreamingBitrate`. Streams have
  `Index=-1`. `ReadAtNativeFramerate`/`IgnoreIndex` are commented out (336-337),
  so the HDHR host ignores the tuner's `ReadAtNativeFramerate` flag.
- **Opening** (`GetChannelStream`, 385-454): tuner limit check (see 2.7). Profile
  = left part of `streamId` before `_`; forced `native` if model has no transcode.
  - Legacy: `HdHomerunUdpStream` with `LegacyHdHomerunChannelCommands(path)`
    (regex `\/ch([0-9]+)-?([0-9]*)` → `channel`, `program`).
  - HTTP: `Protocol=Http`, `Path = channel.URL` + `?transcode=<profile>` when not
    native → `SharedHttpStream`. `AllowStreamSharing` is **not** consulted for HDHR.
- `Validate` (456-479) clears model cache and fetches discover.json (404 ok).
- `HdHomerunUdpStream.cs`: picks a free local UDP port in 49152-65535 (59-71,
  note `Enumerable.Range(min, max)` takes *count* = max, a latent bug), opens TCP
  to device port 65001 to learn local IP, then `HdHomerunManager.StartStreaming`
  sets `/tunerN/lockkey`, `channel`/`program`, `target rtp://<local>:<port>` on
  the first free tuner (loops `numTuners`; `CheckTunerAvailability` reads
  `lockkey`; throws `LiveTvConflictException("No tuners available")`,
  `HdHomerunManager.cs:76-148`). RTP payload is written (12-byte header stripped)
  to the temp file; 30 s receive timeout (186-217). Path rewritten to
  `/LiveTv/LiveStreamFiles/<UniqueId>/stream.ts` (150). `HdHomerunChannelCommands`
  (`vchannel … transcode=`) and `HdHomerunManager.ChangeChannel` exist but are unused.

### 2.5 M3U (`LTV/TunerHosts/M3UTunerHost.cs`, `M3uParser.cs`)

`Type = "m3u"`, `Name = "M3U Tuner"`. Channel id prefix =
`"m3u_" + md5(tuner.Url)` (64-67); final id = prefix + `md5(stream url)`
(`M3uParser.cs:104`). `DiscoverDevices` returns empty (213-216).

**Parser** (`M3uParser.cs`):
- Source: local file if `Url` does not start with `http`, else HTTP GET with
  optional `UserAgent` header, `ResponseHeadersRead` (48-70).
- Skips blank lines and `#EXTM3U`; `#EXTINF:` sets pending info; next non-`#`
  line is the URL (72-114). URL schemes accepted: `http, https, rtsp, rtp, udp`
  only (257-265); others are skipped with a warning.
- Attribute regex `([a-z0-9\-_]+)="([^"]+)"` (36). Attributes used:
  - `tvg-logo`, else `logo` → `ChannelInfo.ImageUrl` (128-135).
  - `group-title` → `ChannelGroup` (137-140).
  - `tvg-id`, else `channel-id` → `TunerChannelId` (EPG match key) (145-149).
  - Lines 151-165 also build `Id` from `channel-id_tvg-id`, but the caller
    overwrites `Id` at line 104 (dead code).
  - Name: text after last `,`, minus a leading SatIP-style number (`84. VOX` /
    `84.0 - VOX`); fallback `tvg-name`, then `tvg-id` (279-320).
  - Number (170-255): `tvg-chno` if numeric; else numeric `tvg-id`, else numeric
    `channel-id`; else leading number in name; else URL file name without
    extension if numeric. `0`, `-1`, empty are invalid.
- Not parsed: `tvg-shift`, `catchup*`, `tvg-rec`, `#EXTGRP`, `#EXTVLCOPT`, `tvg-url`.

**Media source** (`CreateMediaSourceInfo`, 144-211): `Protocol` from path;
`IsRemote` = host not in LAN; HTTP `User-Agent` = tuner `UserAgent` or a fixed
Chrome 122 UA (163-166) in `RequiredHttpHeaders`; video stream `Index=-1,
IsInterlaced=true`, audio `Index=-1`; `RequiresOpening/RequiresClosing=true`;
`RequiresLooping = EnableStreamLooping`; `ReadAtNativeFramerate`;
`Id = md5(path)`; `IsInfiniteStream=true`; `IgnoreDts`; `SupportsDirectPlay =
!EnableStreamLooping && TunerCount == 0`; `SupportsDirectStream =
!EnableStreamLooping`; `UseMostCompatibleTranscodingProfile =
!AllowFmp4TranscodingContainer`; `FallbackMaxStreamingBitrate`.

**Opening** (`GetChannelStream`, 78-130): tuner limit (2.7); then if
`AllowStreamSharing && Protocol == Http && !RequiresLooping`: if URL path has an
extension in `.ts, .tsv, .m2t` → `SharedHttpStream`; if no extension, HEAD
request and share only if `Content-Type: video/MP2T`. Otherwise a plain
`LiveStream` (no buffering; ffmpeg reads the upstream URL directly).

`Validate` = open the listing stream (132-137).

### 2.6 `LiveStream` / `SharedHttpStream` / `ExclusiveLiveStream`

- `LiveStream` (`LTV/TunerHosts/LiveStream.cs`): `UniqueId = Guid "N"`,
  `ConsumerCount = 1`, `EnableStreamSharing = true` (by default, even for the
  non-buffering M3U case), `TunerHostId`, buffer file
  `TempFilePath = <TranscodePath>/<UniqueId>.ts` (77-80). `Open` just sets
  `DateOpened`. `Close` sets `EnableStreamSharing=false` and cancels the CTS
  (88-95). `GetStream` opens the temp file `FileShare.ReadWrite`; if the stream
  is older than 10 s it seeks to `End - 20000` bytes (97-114) so late joiners
  start near live. `DeleteTempFiles` retries 40 x 500 ms (131-151).
- `SharedHttpStream` (`SharedHttpStream.cs`): `Open` GETs the upstream
  (`ResponseHeadersRead`, `CancellationToken.None`), starts a background
  `Task.Run` copying to the temp file; `Open` waits for the first bytes
  (`Resolve` via `StreamHelper.CopyToAsync` `onStarted`), then rewrites
  `MediaSource.Path = <local api url>/LiveTv/LiveStreamFiles/<UniqueId>/stream.ts`,
  `Protocol = Http` (44-75). Zero bytes → `EndOfStreamException`. On end/cancel/error:
  `EnableStreamSharing=false`, delete temp file (77-127). The temp file grows
  without bound for the stream's life; there is no ring buffer.
- `ExclusiveLiveStream` (`LTV/IO/ExclusiveLiveStream.cs`): wrapper for non-
  `ISupportsDirectStreamProvider` services; `EnableStreamSharing=false`,
  `GetStream` throws.

### 2.7 Tuner count and stream sharing

Order of operations when a channel stream is opened
(`DefaultLiveTvService.GetChannelStreamWithDirectStreamProvider`, 462-502):

1. If `streamId` (the media source id, e.g. `native_<md5>_<md5>` or `md5(url)`)
   matches an open `ILiveStream.OriginalStreamId` whose `EnableStreamSharing` is
   true → `ConsumerCount++` and return it. No tuner is used.
2. Else, for each `ITunerHost`, call `GetChannelStream`. `FileNotFoundException`
   (wrong prefix) and `OperationCanceledException` move on; other exceptions
   propagate. None → `ResourceNotFoundException("Tuner not found.")`.
3. Inside the host: if `TunerCount > 0` and the count of open `ILiveStream`s with
   this `TunerHostId` is `>= TunerCount` → `LiveTvConflictException`
   (`HdHomerunHost.cs:387-398`, `M3UTunerHost.cs:80-91`). For HDHR, `TunerCount`
   is the configured value (filled from discover.json at discovery time).

Consequences: sharing happens only for the identical media source id (same
channel + same HDHR profile). A different transcode profile on the same channel
uses another tuner. Recordings and viewers share the same `ILiveStream` when the
ids match.

### 2.8 Open / close path (MediaSourceManager, MediaInfoController)

**Media sources for a channel** (`LiveTvMediaSourceProvider.GetMediaSources`,
53-128): only for `SourceType.LiveTV` items with empty `Path` or an active
recording. For channels: `service.GetChannelStreamMediaSources` →
`Normalize` (142-255: sets `IsInfiniteStream=true`, fills dummy video+audio
streams with `Index=-1`, nulls `<= 0` stream values, sets all indexes to -1 on
duplicates, `InferTotalBitrate`). Each source: `Type=Default`,
`BufferMs ??= 1500`, and if `RequiresOpening`:
`OpenToken = "<ItemTypeName>_<itemId N>_<sourceId>"` (`StreamIdDelimiter='_'`,
"Do not use a pipe … Roku", 27). `MediaSourceManager.SetKeyProperties`
(`MediaSourceManager.cs:356-369`) then prefixes `OpenToken` and `LiveStreamId`
with `md5(provider FullName) + "_"`.

`LiveTvChannel.GetMediaSources` (`MediaBrowser.Controller/LiveTv/LiveTvChannel.cs:125-140`)
returns only a `Placeholder` source (`IsInfiniteStream = RunTimeTicks is null`).

**Open** — `MediaSourceManager.OpenLiveStreamInternal` (`MediaSourceManager.cs:634-697`):
1. Under global `_liveStreamLocker`, resolve provider from `OpenToken` prefix
   (`GetProvider`, 1019-1032), call `provider.OpenMediaSource(keyId, currentLiveStreams)`.
   `LiveTvMediaSourceProvider.OpenMediaSource` (131-140) splits the token into
   `[type, itemId, mediaSourceId]` and calls `GetChannelStream` (259-306): looks
   up the `LiveTvChannel`, calls the service, sets `RequiresClosing=true`,
   `LiveStreamId = md5(service FullName) + "_" + info.Id`, `Normalize`.
2. Re-check `SupportsDirectStream`; `SetKeyProperties`; store in
   `_openStreams[LiveStreamId]` (61, 657).
3. Probe: if any stream has `Index != -1` or `!SupportsProbing` → `AddMediaInfo`
   (estimates bitrate by width: ≥3000→30 Mb, ≥1900→20 Mb, ≥1200→8 Mb, ≥700→2 Mb;
   `RunTimeTicks=null` when infinite, 699-748). Else
   `LiveStreamHelper.AddMediaInfoWithProbe(cacheKey = OpenToken, addProbeDelay=true)`
   (`Emby.Server.Implementations/Library/LiveStreamHelper.cs:38-184`): JSON cache
   at `cache/mediainfo/<md5(openToken)>.json`; if not cached, waits
   `max(3000, AnalyzeDurationMs)` ms, sets `AnalyzeDurationMs=3000`, ffprobes,
   caches. With a cache key it keeps only the first video + first audio stream,
   sets `Index=-1`, `Language=null` (105-118); nulls `RunTimeTicks` if originally
   null; `videoStream.IsAVC = null` ("coming up false and preventing stream
   copy", 176-177); `InferTotalBitrate(true)`.
4. Clone via JSON, apply default audio/subtitle indices for the user, return
   `LiveStreamResponse` + `IDirectStreamProvider`.

**API open**:
- `POST /LiveStreams/Open` (`MediaInfoController.cs:269-306`): query or body
  (`OpenLiveStreamDto`) `openToken, userId, playSessionId, maxStreamingBitrate,
  startTimeTicks, audioStreamIndex, subtitleStreamIndex, maxAudioChannels, itemId,
  enableDirectPlay, enableDirectStream, alwaysBurnInSubtitleWhenTranscoding`,
  body-only `DeviceProfile`, `DirectPlayProtocols` (default `[Http]`).
  → `MediaInfoHelper.OpenMediaSource` (`Jellyfin.Api/Helpers/MediaInfoHelper.cs:414-463`):
  uses request profile or the device's stored capabilities profile; runs
  `SetDeviceSpecificData` (direct play/stream/transcode decision); if no profile,
  appends `&LiveStreamId=<id>` to `TranscodingUrl`; `NormalizeMediaSourceContainer`.
- `POST /Items/{itemId}/PlaybackInfo` with `AutoOpenLiveStream=true`
  (`MediaInfoController.cs:116-260`): after building sources, if the chosen
  source `RequiresOpening` and has no `LiveStreamId`, opens it and replaces
  `MediaSources` with the single opened source (219-247). With `liveStreamId`
  supplied, `GetPlaybackInfo` returns `GetLiveStream(liveStreamId)` only
  (`MediaInfoHelper.cs:111-116`).
- Lazy open in transcoding: `TranscodeManager.AcquireResources`
  (`MediaBrowser.MediaEncoding/Transcoding/TranscodeManager.cs:663-685`) opens
  the stream if `RequiresOpening` and the request has no `LiveStreamId`, then
  waits `BufferMs`.

**Read**: `StreamingHelpers.GetStreamingState`
(`Jellyfin.Api/Helpers/StreamingHelpers.cs:116-172`): with `LiveStreamId`,
`GetLiveStreamWithDirectStreamProvider`; missing → 404 ("client keeps polling
live.m3u8 after the stream was disposed"). Caps `VideoBitRate` at
`FallbackMaxStreamingBitrate` (156-160). Default container `.ts` for live/infinite.
`AttachMediaSourceInfo` prefers `EncoderPath/EncoderProtocol` over `Path`.

**Close** — `MediaSourceManager.CloseLiveStream(id)` (996-1017): under lock,
`ConsumerCount--`; at `<= 0` remove from `_openStreams` and `liveStream.Close()`.
Callers:
- `POST /LiveStreams/Close?liveStreamId=` (`MediaInfoController.cs:314-320`).
- `SessionManager.CloseLiveStreamIfNeededAsync` (`Emby.Server.Implementations/Session/SessionManager.cs:325-362`)
  with `_activeLiveStreamSessions` (67, 861-) mapping session/playSession ids.
  Called on playback stopped (1139), negative-position stop (1066), session end (317).
- `TranscodeManager` kill job with `closeLiveStream` (`TranscodeManager.cs:243-246`).
- `RecordingsManager.RecordStream` end (386-396).
- `MediaSourceManager.Dispose` closes all (1043-1056).

**DirectStreamProvider**: `ILiveStream` implementations that also implement
`IDirectStreamProvider` (`HdHomerunUdpStream`; `LiveStream` exposes
`GetStream()`) let `DirectRecorder` and `/LiveTv/LiveStreamFiles` read the
buffer file directly.

**Stream file routes** (no `[Authorize]` → anonymous; OpenAPI `security: []`):
- `GET /LiveTv/LiveStreamFiles/{streamId}/stream.{container}`
  (`LiveTvController.cs:1145-1161`): `streamId` = `ILiveStream.UniqueId`
  (`GetLiveStreamInfoByUniqueId`, `MediaSourceManager.cs:959-962`); returns
  `ProgressiveFileStream(liveStream.GetStream())` with MIME from `file.<container>`.
  `container` validated by `EncodingHelper.ContainerValidationRegexStr`.
- `GET /LiveTv/LiveRecordings/{recordingId}/stream` (1118-1132):
  `RecordingsManager.GetActiveRecordingPath(id)` → `ProgressiveFileStream(path, null, transcodeManager)`.
  `ProgressiveFileStream` (`MediaBrowser.Controller/Streaming/ProgressiveFileStream.cs`)
  tails a growing file, 50 ms polls, 30 s idle timeout.

---

## 3. Listings providers and guide refresh

### 3.1 `ListingsProviderInfo` (`livetv.xml` → `ListingProviders`)

`MediaBrowser.Model/LiveTv/ListingsProviderInfo.cs`: `Id, Type ("xmltv" |
"SchedulesDirect"), Username, Password, ListingsId, ZipCode, Country, Path,
EnabledTuners[], EnableAllTuners (true), NewsCategories ["news","journalism",
"documentary","current affairs"], SportsCategories ["sports","basketball",
"baseball","football"], KidsCategories ["kids","family","children","childrens",
"disney"], MovieCategories ["movie"], ChannelMappings (NameValuePair[]),
MoviePrefix, PreferredLanguage, UserAgent`.

`MoviePrefix` is used only to strip a name prefix for metadata lookup
(`MediaBrowser.Controller/LiveTv/LiveTvProgram.cs:236-252`). `PreferredLanguage`
used by XMLTV only. `UserAgent` is not used by the XMLTV downloader.

### 3.2 `ListingsManager` (`LTV/Listings/ListingsManager.cs`)

- `SaveListingProvider` (55-84): `provider.Validate(info, validateLogin,
  validateListings)`, new `Guid "N"` id, save, invalidate caches (in-memory EPG
  channel cache + `cache/xmltv/<id>.xml`, 335-362), queue guide refresh.
  Controller hashes `pw` query with SHA1 hex lower before saving
  (`LiveTvController.cs:1003-1006`).
- `GetProgramsAsync(channel, start, end)` (119-174): for each listing provider
  enabled for the channel's tuner (`EnableAllTuners` or `EnabledTuners` contains
  `TunerHostId`, 310-320): resolve EPG channel, fetch programs, then **rewrite**
  `program.ChannelId = tuner channel id`, `program.Id += "_" + channel.Id`.
  **First provider that returns any programs wins.**
- `AddProviderMetadata` (177-204, 286-308): copies EPG channel `ImageUrl` onto
  tuner channels.
- EPG channel match (`GetEpgChannelFromTunerChannel`, 387-455), in order, each
  through `ChannelMappings` (`Name` = tuner key → `Value` = provider id):
  1. tuner `ChannelInfo.Id` → EPG id;
  2. `TunerChannelId` (M3U `tvg-id`; strips `.json.schedulesdirect.org` and leading `I`) → EPG id;
  3. `Number` → EPG number;
  4. normalized name (spaces and `-` removed, `EpgChannelData.cs:49-52`) → EPG name.
- `GetChannelMappingOptions(providerId)` (207-233): tuner channels (fresh,
  `enableCache=false`) for that provider, provider channels, current mappings,
  `ProviderName`. Each `TunerChannelMapping` = `{Name: "<Number> <Name>", Id:
  tuner channel id, ProviderChannelName, ProviderChannelId}` (457-478).
- `SetChannelMapping(providerId, tunerChannelId, providerChannelId)` (236-275):
  drop existing mapping for the tuner id; add a new pair unless the two ids are
  equal or the pair already existed (so posting an existing pair *removes* it);
  save; queue guide refresh; return the updated mapping for that tuner channel.
  Body DTO `Jellyfin.Api/Models/LiveTvDtos/SetChannelMappingDto.cs`
  (`ProviderId, TunerChannelId, ProviderChannelId`).

### 3.3 XMLTV (`LTV/Listings/XmlTvListingsProvider.cs`)

- `Type = "xmltv"`, `Name = "XmlTV"`. `Path` = URL or local file.
- Download cache `cache/xmltv/<providerId>.xml`, max age **1 hour** (29, 59-115).
  Follows redirects; gunzips when the (redirected) URL ends `.gz`/`.gzip`
  (117-160). Empty file → error. Validate: only checks local file exists (282-291).
- Parsing via `Jellyfin.XmlTv.XmlTvReader(path, language)`; language =
  `PreferredLanguage` or server `PreferredMetadataLanguage` (49-57). The whole
  file is re-opened and re-scanned **per channel** (`GetProgramsAsync`, 162-177).
- Program mapping (`GetProgramInfo`, 201-280): `Name=title`, `Overview=desc`,
  `EpisodeTitle=episode.title`, `SeasonNumber/EpisodeNumber` from episode-num,
  `Genres=categories`, `ProductionYear=copyright date year`, `IsSeries = episode
  number present`, `IsRepeat = previously-shown && !new`, `IsPremiere`,
  `IsLive`, Kids/Movie/News/Sports from category lists, `ImageUrl = first icon`,
  `ThumbImageUrl = first image type Still`, `BackdropImageUrl = Backdrop`,
  `OfficialRating = first rating`, `CommunityRating = first star-rating`,
  `SeriesId = md5(title)` when series. `ShowId` = XMLTV programme id, else
  `md5(...)` (bug: lines 246/251 assign instead of append, so the season/episode
  value replaces the title string), plus start ticks for new series episodes
  without episode number. `Id = "<channel>_<start:O>"`. Movies clear series fields.
  Etag via `XmlTvProgramEtag.TryCreate` (179-199).
- Channels (`GetChannels`, 305-321): `Id`, `DisplayName`, first icon,
  `Number = number ?? id`. `GetLineups` returns channels as NameIdPairs.

### 3.4 Schedules Direct (`LTV/Listings/SchedulesDirect.cs`, 1104 lines)

`Type = "SchedulesDirect"`, API `https://json.schedulesdirect.org/20141201`
(38). Token via `POST /token` with SHA1 password (765-767), token reuse < 20 h
(630). `POST /schedules`, `POST /programs`, `POST /metadata/programs/` in
batches of 500 (489-502), `GET /headends?country=&postalcode=`, `PUT/GET
/lineups`, `GET /available/countries` (cached 7 days, `CountryCacheDays`, 39,
837-862). Images: primary 2:3, thumb and backdrop 16:9 (184-199); a daily image
limit flag (`IsImageDailyLimitActive`, 922) stops SD image downloads until 00:00 UTC.

### 3.5 `GuideManager` and refresh (`LTV/Guide/`)

- Task `RefreshGuideScheduledTask.cs`: `Name "Refresh Guide"`, `Key
  "RefreshGuide"`, `Category "Live TV"`, hidden when there is one service and no
  tuner hosts (47), default trigger interval **24 h** (63-71).
- `GuideManager.RefreshGuide` (101-165): `CreateRecordingFolders` →
  `ScanForTunerDeviceChanges` → per service `RefreshChannelsInternal` → if all
  services succeeded, `CleanDatabase` deletes `LiveTvChannel`/`LiveTvProgram`
  items not seen this run → `DefaultLiveTvService.RefreshSeriesTimers` and
  `RefreshTimers`.
- Guide days: `LiveTvOptions.GuideDays` clamped to 1..**14**
  (`MaxGuideDays`, 28), default **7** (167-174). Window = `now-1h` to
  `start+days` (233-234).
- `RefreshChannelsInternal` (176-335): channels → `LiveTvChannel` items under the
  "Live TV" named view (`GetInternalLiveTvFolder`, `LiveTvManager.cs:1263-1267`);
  per channel, fetch programs, diff against existing by item id; new →
  `CreateItems`, updated → `UpdateItemsAsync`; Etag provider id `ProgramEtag`
  short-circuits unchanged XMLTV programs (491); channel flags
  `IsMovie/IsNews/IsSports/IsSeries` and `Kids` tag aggregated from its programs;
  channel `RefreshMetadata(ForceSave)`.
- `GetChannel` (381-468): provider id `ExternalServiceId` = service name;
  updates `ExternalId`, `Number`, `Name`, image (`LiveTvChannelImageHelper`).
- Images (`UpdateImages`, 654-727): `Primary←ImageUrl/ImagePath`,
  `Thumb←ThumbImageUrl`, `Logo←LogoImageUrl`, `Backdrop←BackdropImageUrl`.
  `PreCacheImages` (729-786): converts remote images to local for programs
  ending within `MaxCacheDays = 2` (48), parallelism `min(CPU, 10)`, skips SD
  images under the daily limit. Channel logos come from tuner (`tvg-logo`) or
  EPG channel icon (`AddProviderMetadata`).
- `GetGuideInfo` (88-98): `StartDate=now`, `EndDate=now+guideDays`.

---

## 4. Recordings / DVR

### 4.1 Persistence

- Timers: `data/livetv/timers.json` (`LTV/Timers/TimerManager.cs:25`).
- Series timers: `data/livetv/seriestimers.json` (`SeriesTimerManager.cs:16`).
- Both via `ItemDataProvider<T>` (`LTV/Timers/ItemDataProvider.cs`): whole JSON
  array rewritten on every change; in-memory list with lock.

### 4.2 `TimerManager` (`LTV/Timers/TimerManager.cs`)

One `System.Threading.Timer` per `TimerInfo` (20). `AddOrUpdateSystemTimer`
(89-109): skip `Completed/Cancelled`; fire time =
`StartDate - PrePaddingSeconds`; if already past → fire `TimerFired`
**synchronously now**; else schedule. `RestartTimers` on host start
(`RecordingsHost.StartAsync`, `RecordingsHost.cs:29-33`).

### 4.3 `DefaultLiveTvService` timer logic (`LTV/DefaultLiveTvService.cs`)

- `CreateTimer` (215-261): de-dup by `ProgramId` (re-arms Cancelled/Completed,
  else `ArgumentException`), new `Guid "N"` id, copy program info (by program id,
  else channel + start ±3 min, 621-642), `IsManual=true`, add, `TimerCreated`.
- `CreateSeriesTimer` (263-310): `SeriesId = program.ExternalSeriesId` (throws if
  the program is unknown); existing timers with same program/series get
  `SeriesTimerId` and `IsManual=true`; `UpdateTimersForSeriesTimer`.
- `GetTimersForSeries` (806-834): `LiveTvProgram` with
  `ExternalSeriesId = SeriesId` (or `Name` if no series id), `MinEndDate=now`,
  channel filter unless `RecordAnyChannel`. Timer id =
  `md5(seriesTimerId + program.ExternalId)` (867).
- `ShouldCancelTimerForSeriesTimer` (644-669): manual → keep;
  `!RecordAnyTime` and start time-of-day differs ≥ 10 min → cancel;
  `RecordNewOnly && IsRepeat` → cancel; `!RecordAnyChannel` and other channel →
  cancel; `SkipEpisodesInLibrary && IsProgramAlreadyInLibrary` → cancel.
- `IsProgramAlreadyInLibrary` (964-1000): only S/E match against a `Series` with
  the same name and a non-virtual `Episode`; episode-title-only returns false.
- Duplicate show handling when `SkipEpisodesInLibrary` (671-707): group by
  `ShowId` (skip ids ending `0000`, issue #5856); keep one, preferring HD channel
  then earliest start; others `Cancelled`.
- `UpdateTimersForSeriesTimer` (709-804): add new timers, update metadata on
  inactive existing timers; with `deleteInvalidTimers`, cancel future `New`
  timers that no longer match.
- `RefreshTimers` (88-117): delete ended non-active timers and timers whose
  program vanished; refresh program metadata.
- `GetNewTimerDefaultsAsync` (405-441): padding from config (`max(0, …)`),
  `RecordAnyChannel=false`, `RecordAnyTime=true`, `RecordNewOnly = !program.IsRepeat`,
  all 7 days, `SkipEpisodesInLibrary = RecordNewOnly`, `KeepUntil=UntilDeleted`.
- `UpdateTimerAsync` (342-363): only padding fields, only when not recording.
- `UpdateSeriesTimerAsync` (312-340): copies `ChannelId, Days, EndDate,
  IsPost/PrePaddingRequired, Post/PrePaddingSeconds, Priority, RecordAnyChannel,
  RecordAnyTime, RecordNewOnly, SkipEpisodesInLibrary, KeepUpTo, KeepUntil,
  StartDate` and re-evaluates timers.
- Cancel (147-203): series cancel deletes child timers; single cancel of a series
  child marks it `Cancelled` and keeps it (so it is not recreated); calls
  `RecordingsManager.CancelRecording`.
- `OnTimerManagerTimerFired` (540-596): end = `EndDate + PostPaddingSeconds`;
  past → delete; already active → skip; refresh program info;
  `RecordingsManager.RecordStream(info, channel, end)`. `async void`.
- `ResetTuner` and `CloseLiveStream` are no-ops (530-538).

Stored but **not enforced** anywhere in tree: `SeriesTimerInfo.Days`, `Priority`,
`KeepUntil`, `IsPrePaddingRequired`, `IsPostPaddingRequired`
(only copied in `LiveTvDtoService` / `DefaultLiveTvService`).

### 4.4 `RecordingsManager` (`LTV/Recordings/RecordingsManager.cs`)

- Active recordings: `ConcurrentDictionary<timerId, ActiveRecordingInfo>` (54).
- Paths: default `RecordingPath` or `data/livetv/recordings` (106-116).
- `RecordStream` (304-425):
  1. `FetchInternetMetadata` for series with `SeriesProviderIds` (remote series
     search; used for the folder year) (453-474).
  2. `GetRecordingPath` (476-585), see 4.6.
  3. `GetPlaybackMediaSources(channel)`, take `[0]`; if `RequiresOpening`,
     `OpenLiveStreamInternal` (shares with viewers when ids match).
  4. `GetRecorder` (791-801): `EncodedRecorder` if `RequiresLooping`, or
     container not ending in `ts`, or protocol not File/Http; else `DirectRecorder`.
  5. `EnsureFileUnique` appends ` - N` (771-789).
  6. `onStarted`: set path, add active, status `InProgress`, save NFO/images,
     `CreateRecordingFolders`, queue refresh of parent folders, `EnforceKeepUpTo`.
  7. Record for `recordingEndDate - now`.
  8. Close live stream; delete 0-byte file; refresh.
  9. On failure with time left and `RetryCount < 10`: retry in 60 s
     (`PrePaddingSeconds=0`, `StartDate=now+60s`) (403-413). Success with file →
     `Completed`, `RecordingPath`, `PostProcessRecording`. No file → delete timer.
- `EnforceKeepUpTo` (653-728): when the series timer has `KeepUpTo > 0`, delete
  older completed timers' files beyond `KeepUpTo - 1` and older library items in
  the series folder (by `DateCreated`), with `DeleteFileLocation=true`.
- `PostProcessRecording` (803-837): runs `RecordingPostProcessor` with
  `RecordingPostProcessorArguments` (`{path}` replaced; default `"{path}"`),
  `UseShellExecute=false`, waits for exit, logs exit code.
- `GetActiveRecordingInfo(path)` (123-140): used by
  `LiveTvMediaSourceProvider` so an in-progress recording item plays from
  `/LiveTv/LiveRecordings/{id}/stream` (`MediaSourceManager.GetRecordingStreamMediaSources`,
  `MediaSourceManager.cs:970-994`: `EncoderPath` = that URL, `Protocol=File`,
  `IsInfiniteStream=true`, `IgnoreDts`, `IgnoreIndex`, probed).

### 4.5 Recorders (`LTV/IO/`)

- `DirectRecorder.cs`: with `IDirectStreamProvider`, copies
  `ProgressiveFileStream(provider.GetStream())` to the target (`FileMode.CreateNew`)
  with `emptyReadLimit=1000`; otherwise HTTP GETs `mediaSource.Path` and
  `CopyUntilCancelled`. Duration enforced by a linked CTS. No ffmpeg.
- `EncodedRecorder.cs`: despite the name, **stream copy** with ffmpeg:
  `[-async 1 -fflags +igndts+ignidx+genpts] [-re [-readrate_catchup 100 if ffmpeg>=8]]
  [-stream_loop -1 -reconnect_at_eof 1 -reconnect_streamed 1 -reconnect_delay_max 2]
  -analyzeduration 5000000 -i "<path>" -codec:v:0 copy -fflags +genpts -map_metadata -1
  -threads N -codec:a:0 copy -sn -y "<target>.ts"` (130-200). Log at
  `log/record-transcode-<guid>.txt` with media source JSON header. Stop = write
  `q` to stdin, wait 10 s, kill (210-260). Non-zero exit → `FfmpegException`.
  It does not pass `RequiredHttpHeaders` (User-Agent) to ffmpeg. Output always
  `.ts`. `GetOutputSizeParam` (yadif) and `CopySubtitles` are dead code.

### 4.6 Naming (`RecordingHelper.cs`, `RecordingsManager.GetRecordingPath`)

Folder (476-580), with `EnableRecordingSubfolders` adding a type folder:
- Series (`IsProgramSeries`): `SeriesRecordingPath` or default (+`Series`),
  `<Name>[ (<remote series year>)]`, then `Season N` if season known. A custom
  path different from default disables the type subfolder.
- Movie: `MovieRecordingPath` or default (+`Movies`), `<Name>[ (<year>)]`.
- Kids: default (+`Kids`), `<Name>[ (<year>)]`. Sports: (+`Sports`)/`<Name>`.
  Other: (+`Other`)/`<Name>`.
File name (`RecordingHelper.GetRecordingName`, 10-69), `.ts`:
- Series: `Name SxxExx`, else `Name yyyy-MM-dd` (original air date, local) or
  `Name yyyy_MM_dd_HH_mm_ss` (start, local); then ` - EpisodeTitle` (or ` Title`
  after SxxExx) if UTF-8 length < 250.
- Movie with year: `Name (Year)`. Else `Name yyyy_MM_dd_HH_mm_ss`.

### 4.7 NFO and images (`RecordingsMetadataManager.cs`)

`SaveRecordingMetadata` (57-129): program from library by `ExternalId` or a
dummy built from the timer; adds genres Sports / Kids+Children / News.
If `SaveRecordingNFO`: series → `tvshow.nfo` in series folder (only if absent,
131-) + episode NFO beside the file (`<file>.nfo`); non-movie/sports/news →
video NFO with `lockData=true`; movie → video NFO. If `SaveRecordingImages`
(407-496): series → `<file>-thumb.<ext>` (Thumb or Primary); others →
`poster`, `fanart`, `landscape`, `logo` in the folder; remote images are first
converted to local.

### 4.8 Recording folders as libraries

`GetRecordingFolders` (143-179): `Recordings` (default path, no collection type),
`Recorded Movies` (`movies`), `Recorded Shows` (`tvshows`) when custom paths
exist and differ. `CreateRecordingFolders` (182-246): adds missing virtual
folders, records them in `LiveTvOptions.MediaLocationsCreated`, removes paths no
longer configured. Runs at startup, at each recording start, on every `livetv`
config save (445-451) and at guide refresh. `/LiveTv/Recordings` queries library
items (Movie/Episode/Video) inside these folders plus plugin channel recording
folders (`LiveTvManager.GetEmbyRecordingsAsync`, 433-; `GetRecordingFoldersAsync`, 1274-1296).
`DELETE /LiveTv/Recordings/{id}` calls `DeleteItem` with
`DeleteFileLocation = false` (`LiveTvController.cs:782-796`).

### 4.9 Conflicts and limits

There is no scheduling-time conflict detection. Limits apply only at stream open
(`TunerCount`, HDHR lockkey). A failed open makes the recording fail and retry
every 60 s up to 10 times while the program is on. `Priority` is never used.
`LiveTvConflictException` is the only signal; the API does not expose a
"conflicts" view.

### 4.10 Notifications

`RecordingNotifier` (`LTV/Recordings/RecordingNotifier.cs:49-97`) sends
`TimerCreated`, `TimerCancelled`, `SeriesTimerCreated`, `SeriesTimerCancelled`
session messages to users with Live TV access.

---

## 5. `LiveTvOptions` (`config/livetv.xml`)

`MediaBrowser.Model/LiveTv/LiveTvOptions.cs`:

| Field | Type | Default | Used by |
|---|---|---|---|
| `GuideDays` | int? | null → 7, clamp 1..14 | GuideManager |
| `RecordingPath` | string | null → `data/livetv/recordings` | RecordingsManager |
| `MovieRecordingPath` | string | null | RecordingsManager |
| `SeriesRecordingPath` | string | null | RecordingsManager |
| `EnableRecordingSubfolders` | bool | false | RecordingsManager |
| `EnableOriginalAudioWithEncodedRecordings` | bool | false | **unused** |
| `TunerHosts` | TunerHostInfo[] | [] | everywhere |
| `ListingProviders` | ListingsProviderInfo[] | [] | ListingsManager |
| `PrePaddingSeconds` | int | 0 | timer defaults |
| `PostPaddingSeconds` | int | 0 | timer defaults |
| `MediaLocationsCreated` | string[] | [] | CreateRecordingFolders bookkeeping |
| `RecordingPostProcessor` | string | null | PostProcessRecording |
| `RecordingPostProcessorArguments` | string | `"{path}"` | PostProcessRecording |
| `SaveRecordingNFO` | bool | true | RecordingsMetadataManager |
| `SaveRecordingImages` | bool | true | RecordingsMetadataManager |

Related caches/files: `cache/<tunerId>_channels`, `cache/xmltv/<providerId>.xml`,
`cache/mediainfo/<md5(openToken)>.json`, `<transcode>/<uniqueId>.ts` buffers,
`data/livetv/timers.json`, `data/livetv/seriestimers.json`.

---

## 6. API routes

Policies (`Jellyfin.Server/Extensions/ApiServiceCollectionExtensions.cs:77-78`):
`LiveTvAccess` = user permission `EnableLiveTvAccess`; `LiveTvManagement` =
`EnableLiveTvManagement`; `RequiresElevation` = administrator.

### 6.1 `LiveTvController` — 40 route templates, 39 actions

38 appear in `jellyfin-openapi-stable.json`; 2 are hidden obsolete
(`[ApiExplorerSettings(IgnoreApi = true)]`). The 40 includes the misspelled alias
`Tuners/Discvover`.

| # | Method | Path | Policy | Line | Notes |
|---|---|---|---|---|---|
| 1 | GET | /LiveTv/Info | LiveTvAccess | 102 | `LiveTvInfo{Services, IsEnabled, EnabledUsers}` |
| 2 | GET | /LiveTv/Channels | LiveTvAccess | 138 | type, userId, startIndex, isMovie/Series/News/Kids/Sports, limit, isFavorite/Liked/Disliked, image opts, fields, enableUserData, sortBy, sortOrder, enableFavoriteSorting=false, addCurrentProgram=true |
| 3 | GET | /LiveTv/Channels/{channelId} | LiveTvAccess | 218 | |
| 4 | GET | /LiveTv/Recordings | LiveTvAccess | 265 | channelId, userId, startIndex, limit, status, isInProgress, seriesTimerId, image opts, fields, isMovie/Series/Kids/Sports/News, isLibraryItem, enableTotalRecordCount |
| 5 | GET | /LiveTv/Recordings/Series | LiveTvAccess | 336 | obsolete, hidden, empty result |
| 6 | GET | /LiveTv/Recordings/Groups | LiveTvAccess | 366 | obsolete, hidden, empty result |
| 7 | GET | /LiveTv/Recordings/Folders | LiveTvAccess | 382 | |
| 8 | GET | /LiveTv/Recordings/{recordingId} | LiveTvAccess | 406 | |
| 9 | POST | /LiveTv/Tuners/{tunerId}/Reset | RequiresElevation | 435 | no-op in DefaultLiveTvService |
| 10 | GET | /LiveTv/Timers/{timerId} | LiveTvAccess | 452 | |
| 11 | GET | /LiveTv/Timers/Defaults | LiveTvAccess | 468 | `programId` → SeriesTimerInfoDto |
| 12 | GET | /LiveTv/Timers | LiveTvAccess | 488 | channelId, seriesTimerId, isActive, isScheduled |
| 13 | GET | /LiveTv/Programs | LiveTvAccess | 542 | channelIds, userId, min/maxStartDate, hasAired, isAiring, min/maxEndDate, isMovie/…, startIndex, limit, sortBy, sortOrder, genres (pipe), genreIds, image opts, seriesTimerId, librarySeriesId, fields, enableTotalRecordCount |
| 14 | POST | /LiveTv/Programs | LiveTvAccess | 626 | body `GetProgramsDto` |
| 15 | GET | /LiveTv/Programs/Recommended | LiveTvAccess | 694 | |
| 16 | GET | /LiveTv/Programs/{programId} | LiveTvAccess | 749 | |
| 17 | DELETE | /LiveTv/Recordings/{recordingId} | LiveTvManagement | 778 | DeleteFileLocation=false |
| 18 | DELETE | /LiveTv/Timers/{timerId} | LiveTvManagement | 804 | |
| 19 | POST | /LiveTv/Timers/{timerId} | LiveTvManagement | 820 | body TimerInfoDto |
| 20 | POST | /LiveTv/Timers | LiveTvManagement | 835 | body TimerInfoDto |
| 21 | GET | /LiveTv/SeriesTimers/{timerId} | LiveTvAccess | 851 | |
| 22 | GET | /LiveTv/SeriesTimers | LiveTvAccess | 873 | sortBy, sortOrder |
| 23 | DELETE | /LiveTv/SeriesTimers/{timerId} | LiveTvManagement | 893 | |
| 24 | POST | /LiveTv/SeriesTimers/{timerId} | LiveTvManagement | 909 | body SeriesTimerInfoDto |
| 25 | POST | /LiveTv/SeriesTimers | LiveTvManagement | 924 | body SeriesTimerInfoDto |
| 26 | GET | /LiveTv/GuideInfo | LiveTvAccess | 938 | |
| 27 | POST | /LiveTv/TunerHosts | RequiresElevation | 950 | body TunerHostInfo |
| 28 | DELETE | /LiveTv/TunerHosts | RequiresElevation | 962 | `?id=` |
| 29 | GET | /LiveTv/ListingProviders/Default | LiveTvAccess | 976 | `new ListingsProviderInfo()` |
| 30 | POST | /LiveTv/ListingProviders | RequiresElevation | 993 | `pw`, `validateListings`, `validateLogin`, body |
| 31 | DELETE | /LiveTv/ListingProviders | RequiresElevation | 1017 | `?id=` |
| 32 | GET | /LiveTv/ListingProviders/Lineups | LiveTvAccess | 1035 | id, type, location, country |
| 33 | GET | /LiveTv/ListingProviders/SchedulesDirect/Countries | RequiresElevation | 1050 | raw JSON file |
| 34 | GET | /LiveTv/ChannelMappingOptions | RequiresElevation | 1066 | `providerId` |
| 35 | POST | /LiveTv/ChannelMappings | RequiresElevation | 1078 | body SetChannelMappingDto |
| 36 | GET | /LiveTv/TunerHosts/Types | LiveTvAccess | 1089 | NameIdPair[] |
| 37 | GET | /LiveTv/Tuners/Discvover | RequiresElevation | 1101 | alias (route name `DiscvoverTuners`) |
| 38 | GET | /LiveTv/Tuners/Discover | RequiresElevation | 1102 | `newDevicesOnly=false`, IAsyncEnumerable |
| 39 | GET | /LiveTv/LiveRecordings/{recordingId}/stream | **none (anonymous)** | 1118 | |
| 40 | GET | /LiveTv/LiveStreamFiles/{streamId}/stream.{container} | **none (anonymous)** | 1145 | |

`DefaultAuthorization` is also listed in OpenAPI for the LiveTvAccess /
LiveTvManagement routes.

### 6.2 Live-stream routes in `MediaInfoController`

| Method | Path | Policy | Line |
|---|---|---|---|
| POST | /LiveStreams/Open | DefaultAuthorization | 269 |
| POST | /LiveStreams/Close | DefaultAuthorization | 314 (`liveStreamId` required) |
| GET/POST | /Items/{itemId}/PlaybackInfo | DefaultAuthorization | 72 / 116 (`liveStreamId`, `autoOpenLiveStream`) |

### 6.3 `MediaSourceInfo` live fields

`RequiresOpening`, `RequiresClosing`, `OpenToken`, `LiveStreamId`,
`IsInfiniteStream`, `BufferMs`, `RequiresLooping`, `ReadAtNativeFramerate`,
`IgnoreDts`, `IgnoreIndex`, `GenPtsInput`, `AnalyzeDurationMs`, `EncoderPath`,
`EncoderProtocol`, `RequiredHttpHeaders`, `UseMostCompatibleTranscodingProfile`
(StreamBuilder keeps only `ts` transcoding profiles,
`MediaBrowser.Model/Dlna/StreamBuilder.cs:856-859`),
`FallbackMaxStreamingBitrate` (cap in `StreamingHelpers.cs:156-160`),
`IsRemote` (`ForceRemoteSourceTranscoding` permission, `MediaInfoHelper.cs:295`).
`StreamInfo` appends `&LiveStreamId=` to built URLs
(`MediaBrowser.Model/Dlna/StreamInfo.cs:1038-1041`). StreamBuilder never offers
external subtitle delivery for infinite streams (`StreamBuilder.cs:1611`).

---

## 7. Transcoding differences for live TV

`MediaBrowser.Controller/MediaEncoding/EncodingHelper.cs`:
- **Input modifier** (`GetInputModifier`, ~7290-7420): `-analyzeduration` from
  `MediaSource.AnalyzeDurationMs` (live streams set 3000 → `3000000` µs) else
  env/config (`GetFfmpegAnalyzeDurationArg`, 7257-7275); `-probesize` if set.
  RTSP: `-rtsp_transport tcp+udp -rtsp_flags prefer_tcp`. `-async` /
  `-vsync -1` when `ReadInputAtNativeFramerate && !IsSegmentedLiveStream`
  (7458-7464). `-re` when `ReadInputAtNativeFramerate` and not RTSP (7346-7351);
  `-readrate 10` for HLS stream-copy with segment deletion; `-readrate_catchup
  readrate*100` on new ffmpeg. `-fflags`: `+igndts` (`IgnoreDts`; default true
  for both tuner types), `+ignidx`, `+genpts` (if `GenPtsInput` **or video copy**),
  `+discardcorrupt`, `+fastseek` (7372-7402). `RequiresLooping` → `-stream_loop -1
  -reconnect_at_eof 1 -reconnect_streamed 1 -reconnect_delay_max 2` (7414-7417).
- `EncodingJobInfo.IsSegmentedLiveStream` =
  `TranscodingType != Progressive && !RunTimeTicks.HasValue`
  (`EncodingJobInfo.cs:145-146`). Live sources have `RunTimeTicks = null`.
- **Stream copy**: interlaced video is never copied when deinterlacing would
  apply (2408-2412); for live (`LiveStreamId` set) with unknown bitrate, a
  requested bitrate limit does not block copy (2563-2569).
- **Deinterlace**: `EncodingJobInfo.DeInterlace(codec, force)` (554-578): any
  interlaced source is deinterlaced when transcoding
  (`IsDeinterlaceAvailable` passes `force=true`, `EncodingHelper.cs:448-453`).
  Tuner sources start with `IsInterlaced=true`; the probe may replace this.
- **Timestamps** (progressive, 7697-7770): copy with `RunTimeTicks` +
  `CopyTimestamps` → `-copyts -avoid_negative_ts disabled -start_at_zero`; copy
  with no runtime → `-fflags +genpts`. Encoding: `-copyts` only with runtime.
- **HLS** (`Jellyfin.Api/Controllers/DynamicHlsController.cs`):
  `DynamicHlsHelper` (`Jellyfin.Api/Helpers/DynamicHlsHelper.cs:150,188`) points
  the master playlist at `live.m3u8` when `IsSegmentedLiveStream`, and disables
  trickplay (335). `GET /Videos/{itemId}/live.m3u8` (165-345) starts one ffmpeg
  per output path, sets `job.IsLiveOutput = true`, waits for `MinSegments`, and
  serves the ffmpeg-written playlist (`HlsHelpers.GetLivePlaylistText`,
  `Jellyfin.Api/Helpers/HlsHelpers.cs:112-131`, which only rewrites the fMP4 init
  URI). Command line (`GetCommandLineArguments`, 1574-): `-hls_playlist_type
  event -hls_list_size 0` (unbounded event playlist) for live, `vod` for main;
  `-hls_base_url "hls/<name>/"` for event; always `-copyts -avoid_negative_ts
  disabled -max_muxing_queue_size N -f hls -max_delay 5000000 -hls_time X`.
  Segments are served by `HlsSegmentController`
  (`Videos/{itemId}/hls/{playlistId}/{segmentId}.{segmentContainer}`, 126).
  Segment deletion (`TranscodingSegmentCleaner.cs:97-98`, keep ≥ 20 s) is the
  only bound on disk use; the playlist itself still lists every segment.
- **Progressive**: `VideosController` (472-476) streams infinite sources from
  `state.MediaPath` via `ProgressiveFileStream` when static.
- No seeking: `RunTimeTicks=null` disables segment math; `StartTimeTicks` is
  not meaningful for live.

---

## 8. Scheduled tasks and item types

Scheduled tasks:
- `RefreshGuideScheduledTask` — "Refresh Guide", key `RefreshGuide`, category
  "Live TV", every 24 h (`LTV/Guide/RefreshGuideScheduledTask.cs`). Queued (with
  cancel) on any tuner/listing/mapping change.
- `RefreshChannelsScheduledTask` — key `RefreshInternetChannels`, plugin
  channels, 24 h (`LTV/Channels/RefreshChannelsScheduledTask.cs:43-82`). Not tuner TV.
- `ChannelPostScanTask` — plugin channels post-scan. Not tuner TV.

Item classes and `BaseItemKind` (`Jellyfin.Data/Enums/BaseItemKind.cs`):
- `LiveTvChannel` (84) → class `MediaBrowser.Controller/LiveTv/LiveTvChannel.cs`;
  `GetClientTypeName() = "TvChannel"` (118-121), so DTO `Type` is `TvChannel`.
  `SourceType=LiveTV`, `LocationType=Remote`, `MediaType` Audio for radio,
  `IsKids` from tag. Parent = "Live TV" named view (`CollectionType.livetv`).
- `LiveTvProgram` (89) → `LiveTvProgram.cs`; client type `"Program"` (205-207).
  `IsSports/IsLive/IsNews/IsKids/IsPremiere` from tags; `IsAiring`, `HasAired`.
- `TvChannel` (175), `Program` (139), `Recording` (147), `TvProgram` (183):
  enum values only, no classes; they exist so `GetBaseItemKind()`
  (`BaseItem.cs:1961-1964`, parses the client type name) works and for legacy
  Emby clients. Recordings are ordinary `Movie`/`Episode`/`Video` library items.
- Plugin channels use `Channel` / `ChannelFolderItem` kinds (separate feature).

---

## 9. Weaknesses visible in code

1. **No time-shift / pause / rewind.** Buffers are append-only files under the
   transcode path; late joiners seek to `EOF - 20000` bytes
   (`LiveStream.cs:107-111`). No DVR window, no catch-up (M3U `catchup*` ignored).
2. **Unbounded buffer files** per shared stream (`SharedHttpStream`,
   `HdHomerunUdpStream`) for the whole session. Event HLS playlists grow without
   bound (`hls_list_size 0`).
3. **Tuner failover is incidental.** Only hosts with the same channel id are
   retried (HDHR `hdhr_<GuideNumber>`); M3U ids embed the playlist URL hash, so
   duplicate playlists never fail over. No mid-stream failover or reconnect,
   except `EnableStreamLooping` for M3U.
4. **Stream sharing keys on the media source id** only; HDHR transcode profiles
   each use a tuner. M3U non-TS streams fall back to `LiveStream`, which still has
   `EnableStreamSharing = true` and holds a "tuner slot" while each ffmpeg
   connects upstream separately.
5. **Global lock** `_liveStreamLocker` is held for the whole provider open
   (HTTP connect and wait for the first bytes, or HDHR lockkey/RTP start), so one
   slow tuner blocks every other open and close (`MediaSourceManager.cs:639-658`,
   1000). The probe runs after the lock is released.
6. **Probe delay**: every uncached open waits ≥ 3 s before ffprobe
   (`LiveStreamHelper.cs:71-77`); probe results are cached per OpenToken forever
   (`cache/mediainfo`), so a channel that changes codec keeps stale info.
7. **No conflict management.** `Priority`, `Days`, `KeepUntil`,
   `IsPre/PostPaddingRequired` are stored but ignored. Conflicts surface only as
   failed opens with 10 x 60 s retries.
8. **`ResetTuner` and `CloseLiveStream` are no-ops** in `DefaultLiveTvService`
   (530-538); `HdHomerunManager.ChangeChannel` and `HdHomerunChannelCommands`
   are dead code.
9. **XMLTV performance**: the whole XML is parsed once per channel per refresh;
   cache TTL is 1 h; `ShowId` hashing bug (`XmlTvListingsProvider.cs:246,251`);
   listing provider `UserAgent` not applied to XMLTV downloads.
10. **First listing provider wins** per channel; no merging.
11. **Timers persisted as whole-file JSON rewrites**; `TimerFired` may run
    synchronously inside `Add/Update` when the start is in the past;
    `OnTimerManagerTimerFired` is `async void`.
12. **`EncodedRecorder` ignores `RequiredHttpHeaders`** (M3U User-Agent), always
    remuxes to `.ts`, and drops subtitles (`-sn`).
13. **`DELETE /LiveTv/Recordings/{id}` keeps the file** (`DeleteFileLocation=false`).
14. **Anonymous stream endpoints** (`LiveStreamFiles`, `LiveRecordings`) rely on
    unguessable ids (Guid "N" / timer id).
15. **HDHR quirks**: dummy 1920x1080 for HD natives ("android tv's 1200
    condition"), `internet720` never offered, `AllowStreamSharing` ignored,
    UDP port picker uses `Enumerable.Range(min, max)` as count.
16. **M3U parser**: `Id` built from `channel-id`/`tvg-id` is overwritten (dead
    code, `M3uParser.cs:151-165` vs 104); only http/https/rtsp/rtp/udp URLs.
17. **Guide refresh** cancels and re-queues itself if `ScanForTunerDeviceChanges`
    saves a changed HDHR URL (`TunerHostManager.cs:172-178` → `SaveTunerHost` →
    `CancelIfRunningAndQueue`).
18. `EnableOriginalAudioWithEncodedRecordings` and `TunerHostInfo.Source` are
    unused.
