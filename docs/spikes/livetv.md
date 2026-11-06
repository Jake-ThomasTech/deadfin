# Spike: Live TV for Deadfin

Status: investigation, 2026-10-12. The operator decided the plan on
2026-10-13 (§6.3); the phases are filed in the "Live TV" milestone.

Question: what Live TV support could Deadfin offer, and in particular which
Emby Live TV features that Jellyfin lacks keep a community member on Emby?

Short answer:

1. **The anchor is local channels over the internet (§0).** A family
   member watches local news on an Apple TV and in jellyfin-web, with poor
   antenna reception at home. The MVP is the M3U + XMLTV path with live
   HLS sources: the free, ad-supported (FAST) and station streams that carry
   local affiliate news. A remote HDHomeRun reached over a VPN comes next.
   Paid virtual pay-TV services are DRM-protected and out of scope.
2. Deadfin has no Live TV today. Decision D2 deferred it; the 38 `LiveTv`
   operations are spec-shaped stubs and `LiveStreams/Open` has a nil provider
   seam.
3. Jellyfin parity (M3U + HDHomeRun tuners, XMLTV guide, live HLS, simple
   DVR) is a medium-size port. The hard part is not the API. It is a
   non-file input path through `internal/playback` and `internal/ffmpeg`,
   plus a shared stream broker. Upstream does not share HLS sources at all,
   and it does not handle SSAI ad discontinuities, expiring URLs or geo
   errors.
4. The Emby gaps users name most are:
   - time-shift (pause and rewind live TV),
   - tuner handling (honest "all tuners busy", priority, failover),
   - a better guide,
   - commercial detection,
   - series-recording rules that actually work.
   
   Upstream stores many of the series rules (`Priority`, `KeepUntil`, `Days`,
   required padding) but never applies them. Several gaps fit the Jellyfin API
   unchanged, because the fields already exist on the wire.
5. Testing is built on a synthetic limits harness, not on collected
   sources. `tools/fakehdhr` (this PR) drives stock Jellyfin 12.0.0 end to
   end. It also generates:
   - lineups of up to 20 k channels;
   - multi-GB XMLTV guides with real-world quirks;
   - FAST-style HLS channels with SSAI ad breaks, SCTE-35 cues, expiring
     tokens, geo blocks and timestamp jumps;
   - tuner streams with scripted faults and WAN caps;
   - hundreds of cheap concurrent tunes.
   
   §5.2 maps each scenario to the Deadfin metrics that set each phase's
   limits. Real public sources are a sanity check only.

The operator reversed D2 for Live TV and SyncPlay as D20 (§6.0, §6.3).

Companion file: [`livetv-upstream.md`](livetv-upstream.md). It is a line-referenced
map of upstream `src/Jellyfin.LiveTv` at the pin (`b04614e`). This document
cites it as **[U§n]**.

---

## 0. Anchor use case: local channels over the internet

**The household.** The operator wants a family member to watch local
broadcast channels, mainly local news, on an Apple TV (SharkParty or
Swiftfin) and in jellyfin-web, at home or away. Reception at the house is
poor, so a home antenna is not the plan. An antenna also does nothing for
Deadfin itself.

**The route.** Three routes exist. Only the first two are in scope:

| Route | What it is | In scope | Why |
|---|---|---|---|
| **A. Live internet HLS (primary MVP)** | Free, ad-supported (FAST) channels and station streams that carry local affiliate news, through the M3U + XMLTV path | **Yes: phases 1–2** | Plain HLS. It exercises the M3U, XMLTV, live HLS and broker code directly, with no hardware |
| **B. Remote network tuner** | An HDHomeRun at a second location with good signal, reached over a VPN or the WAN | **Yes: phase 3** | A real Deadfin feature: a network tuner that tolerates latency, loss and limited bandwidth |
| C. Paid virtual pay-TV (YouTube TV, Hulu + Live TV, Sling, Fubo, DirecTV Stream) | Authenticated apps with Widevine, FairPlay or PlayReady DRM | **No, permanently** | The streams are DRM-protected and the services are closed. Deadfin does not integrate with them, does not do TV Everywhere sign-in, and does not try to capture them |

### 0.1 Route A: FAST and station HLS

**Where local news is free (checked 2026-10-12; [R] has the links).**

| Service | Local content |
|---|---|
| CBS News local streams | 24/7 local news in 14 markets (2023); also on Pluto TV, Paramount+ and station sites |
| Plex | NBC and Telemundo owned-station local news (LA, NY, Philadelphia, Chicago, SF; March 2025) |
| FOX LOCAL | An app with 17 FOX owned stations; the station follows location |
| NewsON | About 200 local stations; free, no login |
| Local Now | Over 225 markets; local channel by location |
| Samsung TV Plus, The Roku Channel | Local news channels; device-bound apps |

**The terms problem, stated plainly.** None of these services publishes its
HLS URLs for third-party players. Pluto's terms, for example, forbid access
the service does not authorize, and the MPA had a Pluto M3U generator
repository taken down in 2023. The services are free and legal to watch in
their own apps and players. Pulling their streams into another player is a
different question, and their terms usually answer it with no. So:

- Deadfin ships **no** FAST URLs, no playlist generators, and no
  per-service resolvers or scrapers. The repository names these services
  only as the kind of content the feature must handle well.
- The operator configures the M3U and XMLTV sources they choose, locally,
  through the normal Live TV settings or the §5.7 hook. Nothing about those
  sources enters the repository.
- Deadfin's job is to play **any well-formed live HLS** well. The behavior
  that FAST streams show (ad stitching, tokens, geo checks) is modeled in
  fakehdhr (§5.1). That makes the feature testable without any service.

**What FAST streams do to a server, and the Deadfin answer:**

| Behavior | What upstream does | What Deadfin does |
|---|---|---|
| **Live HLS ingest** | An `.m3u8` source is never shared. Only `.ts`/`.tsv`/`.m2t`, or `video/MP2T` on a HEAD, gets `SharedHttpStream`. Every viewer gets a plain `LiveStream`, so each viewer's ffmpeg reads the remote playlist itself (`M3UTunerHost.cs:97-129`) | The broker owns **one** ingest per channel for HLS too, and all viewers and recordings consume it (phase 2) |
| **Remux or transcode for AVPlayer** | ffmpeg copies when codecs allow it, or transcodes; one job per viewer | Apple's HLS spec needs H.264 or HEVC video and AAC, AC-3 or E-AC-3 audio (rules 1.1, 2.5). Most FAST streams are already progressive H.264 + AAC, so **remux** is possible. But the ad creative often changes resolution, frame rate or audio layout, and Apple forbids codec changes at a discontinuity (rule 8.16). Rule: **remux while the source keeps one shape. Normalize (transcode to one fixed output) when the source carries SSAI discontinuities or changes shape.** The harness measures the CPU cost of each (L9) |
| **Discontinuities and SCTE-35** | Nothing specific. ffmpeg's HLS demuxer reads through `EXT-X-DISCONTINUITY`; `+genpts`/`+igndts` smooth timestamps; the output carries no discontinuity tags | Apple requires `EXT-X-DISCONTINUITY` at every encoding break (8.13), the same breaks in every variant (8.15), and `EXT-X-DISCONTINUITY-SEQUENCE` in live playlists (8.17). Channels users report Apple TV freezes at Pluto ad breaks. Deadfin either normalizes the breaks away or carries them with a correct sequence. It keeps the SCTE-35 cue times (`EXT-X-CUE-OUT`/`-IN`, `EXT-X-DATERANGE`) as program markers, which later feed commercial skip in recordings (5f) |
| **Expiring or tokenized URLs** | The M3U is re-read only when the guide refreshes. A token that has expired fails the tune. Emby behaves the same; Channels DVR refreshes daily, and its users script hourly refreshes | On a 403, 404 or 410 from a stream URL, the broker re-reads that M3U source once (single-flight, at most once per 5 minutes per tuner), takes the new URL for the same channel identity, and retries. A 401 stops the source instead (§4.3: "credentials rejected"). A per-tuner "refresh every N minutes" option is added too. No per-service token logic: an operator who needs a resolver runs it as their own local URL |
| **Geo restriction** | The ffmpeg error surfaces as a failed stream | The stream comes from the **server's** location. Deadfin never bypasses a geo check. A 403 gives a clear `PlaybackInfo` error (`NotAllowed`) instead of a dead stream, and repeated failures mark the channel unavailable in the guide |
| **Guide matching to FAST ids** | XMLTV is matched by `tvg-id`, then by name or number; the first source wins | The same matching at parity, plus the merges in gap 1 (§4.2). The same station on two services is one channel identity with two sources, which feeds failover (5g) |

### 0.2 Route B: a remote HDHomeRun, for one household

An HDHomeRun at a location with good signal, such as a relative's house,
joined to the Deadfin network by WireGuard or Tailscale. Deadfin treats it
as a network tuner across a slow, lossy link:

- **Bandwidth.** The FLEX models do not transcode on the device:
  `?transcode=` works on EXTEND models only. The uplink must carry the raw
  stream: ATSC 1.0 HD often runs at 10–15 Mbit/s, and up to 19.39 Mbit/s;
  ATSC 3.0 HEVC runs at about half that. Deadfin measures the tuner's real
  throughput at open. When the throughput is below the stream rate, it
  says so clearly instead of stalling. (A small transcode relay at the
  remote site is a later option, not phase 3.)
- **Latency and loss.** The tuner client needs longer open timeouts,
  larger read buffers, and a reconnect to the same channel after a short
  stall. It must not report a dead tuner on the first hiccup. The UDP
  discovery does not cross a VPN: phase 3 adds tuners by URL.
- **ATSC 1.0 always needs a transcode for Apple.** MPEG-2 video is not
  allowed in Apple HLS (rule 1.1), and interlaced video must be
  deinterlaced (rule 1.14). Upstream uses `deinterlace_qsv=mode=2` on QSV
  (`EncodingHelper.cs:3674`) and `yadif` in software. **Deadfin's QSV and
  NVIDIA filter chains skip the deinterlace branches today**
  (`internal/encoding/hwaccel.go:733,949`, because of divergence D-37:
  Deadfin does not record interlace). Phase 3 must record `IsInterlaced`
  for live probes and port the QSV, VAAPI and software deinterlace
  branches. That is also the busiest QSV path the household will hit.
- **ATSC 3.0 (FLEX 4K, 2 of its 4 tuners).** HEVC needs fMP4 HLS for Apple
  (rule 1.5). **AC-4 audio has no path**: Apple does not decode it, and
  upstream FFmpeg has no AC-4 decoder (ticket #8349 stalled; only the
  librempeg fork has one). SiliconDust's own app converts AC-4 to AC-3 in
  its cloud, which no third-party server can use. Deadfin must mark AC-4
  channels "no audio" instead of failing silently. A3SA DRM channels
  cannot play in any third-party server. Upstream hides them
  (`HdHomerunHost.cs:84`, the lineup's `DRM` flag), and Deadfin does the
  same.
- **Legal frame: personal placeshifting.** One household watches its own
  tuner. Locast retransmitted broadcast channels to the public and lost: on
  31 August 2021 the S.D.N.Y. held that its donation requests were charges
  above cost recovery, so the §111(a)(5) non-profit exemption did not apply.
  Locast stopped on 2 September 2021. Deadfin must never become a
  rebroadcast service. Tuners belong to one server and its household. No
  feature shares channels between servers or to the public, and the docs
  frame the remote tuner as "your tuner, your home".

### 0.3 Away from home

Viewing away from home already uses Deadfin's normal remote-access path.
For live TV that path must hold under a WAN bitrate cap. Deadfin stores
`RemoteClientBitrateLimit` (`internal/config`), but `internal/playback`
does not read it yet; check this before phase 2. The live encode must
respect that limit, and the client's own `MaxStreamingBitrate`. Phase 2
acceptance includes a live channel played through the public ingress with
a 4 Mbit/s cap.

### 0.4 Order

1. **Phase 1:** M3U + XMLTV, with HLS URLs and the refresh hook.
2. **Phase 2:** live playback, with HLS ingest, remux or normalize, and
   discontinuities.
3. **Phase 3:** the remote network tuner: the HDHomeRun API over a VPN,
   deinterlace, AC-4 and DRM handling.
4. **Phase 4:** a simple DVR.

The gaps of §3 follow. §6 has the details.

---

## 1. Current state in Deadfin

### 1.1 Routes

| Surface | State | Where |
|---|---|---|
| 38 `LiveTv` OpenAPI operations | `stubbed` (D2, divergence D-07) | `internal/api/handlers/livetv.go`; `ledger/PARITY.md` §LiveTv (38/38 stubbed) |
| `GET /LiveTv/Info` | `IsEnabled=false`, `Services=[]` | `livetv.go` `info` |
| list routes (Channels, Programs, Recordings, Timers, SeriesTimers, …) | empty `QueryResult` | `livetv.go` `emptyQueryResult` |
| item routes (`Channels/{id}`, `Programs/{id}`, `Recordings/{id}`, `LiveStreamFiles/…`, `LiveRecordings/…`) | 404 `"no tuners configured"` | `livetv.go` `notConfigured` |
| timer and tuner mutations | 204 no-op | `livetv.go` `noContent` |
| admin creates (`TunerHosts`, `ListingProviders`, `ChannelMappings`) | 200 `{}` | `livetv.go` |
| hidden `GET /LiveTv/Recordings/Series`, `/Groups` | `deferred`, not registered | `ledger/annotations.yaml` |
| `POST /LiveStreams/Open` / `Close` | `partial`; Open errors with `ErrLiveStreamUnavailable`, Close is idempotent | `internal/api/handlers/playback_info.go:29-30,156-193`; `internal/playback/playback_info.go:94-98,422-438` |
| `GET /Videos/{id}/live.m3u8` | implemented for **file** sources only | `internal/api/handlers/hls_stream.go:43,164` |

The route-omission fixtures (`cmd/deadfin/testdata/route-omissions.json`)
contain no Live TV entries. Every route is registered.

### 1.2 Code that already anticipates Live TV

- **Item kinds.** `KindLiveTvChannel`, `KindLiveTvProgram`, `KindTvChannel`,
  `KindProgram` and `KindTvProgram` exist (`internal/item/kind.go:28-46`).
  Channel-number sort logic exists too (`internal/item/livetv_number_test.go`).
  `CollectionTypeLiveTV` is a known collection type.
- **Media source DTO fields.** `IsInfiniteStream`, `RequiresOpening`,
  `RequiresClosing` and `LiveStreamId` exist (`internal/playback/models.go:1169-1174`).
  `PlaybackInfo` carries `AutoOpenLiveStream` and `LiveStreamID`
  (`playback_info.go:512-513`).
- **Provider seam.** `playback.LiveStreamProvider` is the intended adapter
  (`playback_info.go:94-98`). Nothing composes it today.
- **Authorization.** `PolicyLiveTVAccess` and `PolicyLiveTVManagement` are in
  the 18-policy matrix. The importer carries `EnableLiveTvAccess` and
  `EnableLiveTvManagement` user policy.
- **Configuration.** `DisableLiveTvChannelUserDataName` is honored for
  user-data keys. Named configuration (`/System/Configuration/{key}`) exists,
  but `livetv` is not registered.

### 1.3 What blocks live playback today

- `validateHLSSource` rejects any source with a `LiveStreamID`, any
  non-`File` protocol, and any remote source
  (`internal/playback/hls_service.go:622-635`).
- `internal/encoding` hard-codes the input as `-i file:<path>`
  (`internal/encoding/hls.go:155`). It has no live input modifiers: no
  `-fflags +igndts/+genpts`, no `-analyzeduration` from the source, no `-re`,
  and no reconnect flags.
- The HLS job model (`internal/ffmpeg/hls_job.go`, `transcode_manager.go`)
  assumes a finite source with `RunTimeTicks`. That drives the segment math,
  seek-by-respawn and the VOD playlist. Upstream switches live sources to an
  ffmpeg-written EVENT playlist ([U§7]).
- No tuner host, listing provider, guide store, timer store or recorder exists.
- `/UserViews` never returns the `livetv` view. Swiftfin and jellyfin-web use
  that view as the Live TV entry point (§2.3).

### 1.4 Docs that touch Live TV

- `docs/00-overview.md` D2: Live TV and SyncPlay are deferred entirely.
- `docs/divergences.md` D-07: the endpoints are stubbed and the UIs hide them.
- `docs/03-api-surface.md` §Stub / deferred policy: `IsEnabled=false`, empty
  lists, and Timer WS events never fire.
- `docs/05-parity-ledger.md`: the `stubbed` status exists for Live TV and SyncPlay.
- `docs/work-packages/P9.md`, `WP-P4-02.md` and `WP-P1-03.md`: stub scope,
  `LiveStreams` stubs and the Live TV policies.

---

## 2. Upstream Jellyfin Live TV (pin `b04614e`)

Full detail with line references: [`livetv-upstream.md`](livetv-upstream.md). Summary:

### 2.1 Architecture

```
TunerHosts (livetv.xml)           ListingProviders (livetv.xml)
  HdHomerunHost  M3UTunerHost        XmlTvListingsProvider  SchedulesDirect
        \            /                        |
     TunerHostManager                   ListingsManager  -- ChannelMappings
             \                               /
              DefaultLiveTvService ("Emby")  -- GuideManager / RefreshGuide task (24 h, 7 days)
              /         |           \
   LiveStreams      TimerManager   RecordingsManager -- DirectRecorder | EncodedRecorder (ffmpeg -c copy)
  (SharedHttpStream, SeriesTimerMgr      |               post-processor, NFO, images
   buffer <transcode>/<id>.ts)           v
        |                         recording folders = ordinary libraries
  LiveTvMediaSourceProvider -> MediaSourceManager -> PlaybackInfo -> DynamicHls live.m3u8 (EVENT)
```

- **Tuner hosts** [U§2]:
  - **HDHomeRun.** UDP discovery is a broadcast to port 65001. After a reply,
    upstream reads `http://<ip>/discover.json` and `lineup.json`. HTTP streams
    use `/auto/v<GuideNumber>`, with `?transcode=<profile>` on "hdtc" models.
    Legacy models use UDP/RTP.
  - **M3U.** The parser reads `tvg-id`, `tvg-logo`, `tvg-chno` and
    `group-title`. It accepts http(s), rtsp, rtp and udp URLs.
  - Per host: `TunerCount`, `AllowStreamSharing`, `EnableStreamLooping`,
    `FallbackMaxStreamingBitrate`, `AllowHWTranscoding`,
    `ReadAtNativeFramerate`, `IgnoreDts` and `AllowFmp4TranscodingContainer`.
- **Live streams** [U§2.6-2.8]:
  - `SharedHttpStream` copies the tuner's TS into a temp file that grows
    without limit. It re-serves the file at
    `/LiveTv/LiveStreamFiles/{id}/stream.ts`. That endpoint is anonymous.
  - Viewers that request the identical media source id share the stream
    (`ConsumerCount`). A late joiner seeks to 20 KB before the end of the file.
  - Close is reference-counted. It runs from `LiveStreams/Close`, playback
    stop, session end and transcode kill.
- **Guide** [U§3]:
  - Sources are XMLTV (file or URL, cached 1 h) and Schedules Direct (account
    plus lineups).
  - The Refresh Guide task runs every 24 h and loads 7 days (1 to 14).
  - Channel matching runs through `ChannelMappings`, in this order: id, tuner
    channel id, number, name.
  - The first listing provider that has programs for a channel wins.
- **DVR** [U§4]:
  - Timers and series timers are whole-file JSON (`data/livetv/*.json`).
  - Series options: `RecordAnyTime`, `RecordAnyChannel`, `RecordNewOnly`,
    `SkipEpisodesInLibrary` and `KeepUpTo`. Pre and post padding are set in
    seconds.
  - `Priority`, `Days`, `KeepUntil` and `IsPre/PostPaddingRequired` are
    **stored but ignored**.
  - Conflicts are not detected when a timer is scheduled. A failed start
    retries every 60 s, up to 10 times.
  - Recordings become ordinary Movie/Episode items in auto-created recording
    libraries. An optional post-processor command runs after each recording.
- **Transcoding** [U§7]:
  - Live inputs get `-analyzeduration 3000000`, `-fflags +igndts` (and
    `+genpts` on video copy), optional `-re`, and reconnect flags when the
    source loops.
  - Every tuner stream is assumed interlaced until the probe says otherwise,
    so transcodes deinterlace with `yadif`.
  - HLS uses `live.m3u8` with `-hls_playlist_type event -hls_list_size 0`.
    Seeking and trickplay are off.

### 2.2 Observed against stock 12.0.0 with `tools/fakehdhr`

`tools/fakehdhr/stock-smoke.sh` runs the pinned image (digest from
`tools/goldenrec/compose.yaml`) against the fake tuner. Observed 2026-10-12:

| Step | Result |
|---|---|
| `POST /LiveTv/TunerHosts {Type:hdhomerun, Url}` | accepted; `DeviceId` read from `discover.json`; stored `TunerCount: 0` (= no server-side limit) |
| `POST /LiveTv/ListingProviders {Type:xmltv, Path:URL}` | accepted; Refresh Guide auto-queued |
| `GET /LiveTv/Channels?addCurrentProgram=true` | 6 `TvChannel` items, `ChannelNumber` "2.1"…, `CurrentProgram` filled, `Primary` logo imported. XMLTV matched by display-name with no manual mapping |
| `GET /UserViews` | a `Live TV` view, `CollectionType: livetv` |
| `GET /LiveTv/GuideInfo` | 7-day window (the guide had 3 days of data) |
| `POST /Items/{channel}/PlaybackInfo {AutoOpenLiveStream:true}` | 200; `Protocol: Http`, `Path: …/LiveTv/LiveStreamFiles/<id>/stream.ts`, `IsInfiniteStream`, `RequiresOpening/Closing`, `LiveStreamId`, `AnalyzeDurationMs: 3000`, probed `mpeg2video 1080i` + `ac3 6ch`, `TranscodingUrl` → `master.m3u8?…LiveStreamId=…` |
| master → `live.m3u8` | `#EXT-X-PLAYLIST-TYPE:EVENT`, 3 s segments |
| ffmpeg | `-analyzeduration 3000000 -probesize 1G -fflags +igndts -i http://…/LiveStreamFiles/…/stream.ts … yadif=0:-1:0 … -hls_playlist_type event -hls_list_size 0` |
| second open of the same channel | shared: same buffer file, no new tuner |
| third channel with both tuners busy | **PlaybackInfo 200** with a source whose buffer file never appears. The tuner's 503 / `805 All Tuners In Use` is swallowed. Log: `Could not find file …ts` and `Error probing live tv stream`. The client sees a dead stream, not an error |
| `POST /LiveStreams/Close` ×N | 204; tuners released |
| `GET /LiveTv/Timers/Defaults?programId=` → `POST /LiveTv/Timers` | recording starts at once; `…/recordings/Fake Cooking/Season 1/Fake Cooking S01E01 Episode 1.ts` + `.nfo` + thumb + `tvshow.nfo`, using `xmltv_ns` |

The busy-tuner row is an upstream defect that users meet in practice (§3).
Deadfin can fix it without a client change: return the existing
`ErrorCode: NoCompatibleStream` PlaybackInfo shape, or open on another tuner.

### 2.3 What our clients call

| Client | Live TV surface | Evidence |
|---|---|---|
| **jellyfin-web 12.0.0** (bundled in the pinned image) | Full: Programs/Guide/Channels/Recordings/Schedule/Series tabs; admin tuner, listings, mapping pages. Calls `getLiveTvPrograms` (12 sites), `getLiveTvChannels` (8), `getRecordingFolders`, `getRecordings`, `getRecommendedPrograms`, `getPrograms` (POST guide), `getTimers`, `getSeriesTimers`, `getDefaultTimer`, `create/cancelTimer`, `create/cancelSeriesTimer`, `getGuideInfo`, `getChannelMappingOptions`, `setChannelMapping`, `addTunerHost`, `discoverTuners` + `Tuners/Discvover`, `resetTuner`, `getLineups`, SchedulesDirect countries, `Recordings/Series`, `Recordings/Groups`, `LiveStreams/Open`, `/System/Configuration/livetv` | `grep` of `/jellyfin/jellyfin-web/*.js` in the pinned image |
| **Swiftfin / SharkParty** (iOS + tvOS) | Live TV tab from the `livetv` user view; channel list (`getLiveTvChannels`), programs and recommended programs, EPG grid (`EPGView`, tvOS `EPGDateBar`), recordings (`getRecordings`), timer and series-timer create/update/cancel (`RecordingTimerViewModel`), in-player EPG supplement behind *Experimental → Live TV EPG*. Playback opens with `PlaybackInfo` `isAutoOpenLiveStream = true` and reports `liveStreamID` in playstate | `SharkParty` `Shared/Objects/Libraries/EPGChannelsLibrary.swift`, `Shared/ViewModels/RecordingTimerViewModel.swift`, `Shared/Objects/MediaPlayerManager/MediaPlayerItem/MediaPlayerItem+Build.swift:77`, `MediaProgressObserver.swift:128-175`, `Shared/Services/SwiftfinDefaults.swift:419` |
| **Jellyfin Android TV** | Native guide grid, channel/program/recording browse, record/series-record dialogs, `LiveStreams/Open` via PlaybackInfo. Not read from source in this spike; see §3 research for user reports | jellyfin-androidtv (not local) |
| **deadfin-web** | None yet; built against the 12.0.0 contract, so the same routes are available | `deadfin-web/README.md` |

The ledger already tags `GetLiveTvPrograms`, `GetRecommendedPrograms` and
`GetRecordings` as Swiftfin traffic (`ledger/annotations.yaml`).

---

## 3. Emby versus Jellyfin gap analysis

Full research with every URL: [`livetv-research.md`](livetv-research.md)
(**[R§n]**). Researched 2026-10-12. Current releases at that date: Jellyfin
12.1/12.2 and Emby 4.9 (4.10 in beta). Reddit could not be crawled. Demand is
ranked from three sources instead:

- features.jellyfin.org votes, read through the Fider API;
- reactions on `jellyfin/jellyfin` issues labelled `live-tv` (131 issues, 27
  open);
- repeated threads on forum.jellyfin.org and emby.media/community.

### 3.1 Ranked gaps

The "Emby" column shows what Emby does today. "Neither" means that Emby lacks
the feature too. Those rows are open ground where Deadfin could lead, not
reasons to stay on Emby.

| # | Gap | Demand | Jellyfin (12.x) | Emby | Key sources |
|---|---|---|---|---|---|
| 1 | **Guide data reliability** | High | Only XMLTV or Schedules Direct (SD) are supported. SD's own home page warns that Jellyfin up to 12.1 gets IPs blocked. Issue #13147 (23 reactions) was fixed in 12.0; #18045 (open) shows a block again after about 24 h, caused by more than 10,000 image requests | Ships its own free guide data for the US, CA and UK, with Premiere | [R§1]; schedulesdirect.org; jellyfin#13147, #18045; emby.media/support/articles/Emby-Guide-Data.html |
| 2 | **Channel groups and channel management** (hide, reorder, renumber, filter by group) | High | None native. The top Live TV feature request (#186 "IPTV Channel Groupings") has 377 votes and has been "planned" since 2025-03. Users run Dispatcharr to filter 11 k-channel playlists | M3U `group-title` becomes tags and guide filter chips; imports can be limited by group; channels can be excluded and reordered | [R§2]; features.jellyfin.org/posts/186, /828, /1910, /3662 |
| 3 | **Guide UI on Apple TV and Roku** (client) | High; likely the community member's case | Swiftfin #842 has been open since 2023. One user there "paid for Emby because of the live tv UI on ATV". A grid EPG ships in Swiftfin 1.7 behind an Experimental setting, and **SharkParty already has it**. On Roku the guide is hidden behind the `*` key and is slow | Grid EPG on Apple TV since 2020; Roku has a full guide plus a mini-guide; Android TV has numeric entry and a previous-channel button | [R§3]; Swiftfin#842; jellyfin-roku#598, #1005 |
| 4 | **Tuner handling** (limits, sharing, release, honest errors) | High/Med | The M3U stream limit is not honored (#10996, open since 2024 and still present in 12.1). Each viewer opens its own upstream; viewers kick each other off without an error. The transcode directory fills the disk (#17120). §2.2 shows that a busy tuner produces a dead stream | One tuner stream is shared per channel. Emby has bugs too (hidden M3U consumers in 4.10) | [R§4]; jellyfin#10996, #17120, #15769; §2.2 |
| 5 | **Time-shift**: pause, rewind, watch from start | Medium | Pause works but rewind does not. A recording that is still in progress cannot be watched from its start. Requests #1142, #1830 and #1950 | A "Live Stream Buffer" setting and time-shift (buggy on some clients). Since 4.8.9 it offers "watch from start" when a channel is recording | [R§5] |
| 6 | **Commercial skipping** | Medium | Not built in. The workaround is comskip → chapters → Chapter Segments Provider → Media Segments (10.10+) | Third-party ComSkipper plugin that reads `.edl` (its Android TV app ignores the seeks). Channels DVR and Plex do this natively | [R§6]; github.com/BillOatmanWork/Emby.ComSkipper |
| 7 | **Series rules and conflicts** | Medium | Has keep-up-to, new-only, padding and skip-if-in-library. **`Priority`, `KeepUntil`, `Days` and required padding are stored but ignored** [U§4.9]. New-only ignores the XMLTV `<new/>` tag. No conflict detection | Adds keyword recording (4.7.7), "check all libraries" and per-series padding. Neither has priority or conflict resolution; Emby has said "planned" since 2019 | [R§7]; features.jellyfin.org/posts/36 |
| 8 | **Logos and mapping** | Med/Low | Logo cache and remap bugs (#15748, the PR #16914 then #17549 revert). Requests: logos from M3U, a "None" mapping option | Per-tuner choice of logo source: guide data or M3U | [R§8] |
| 9 | **Multi-source failover and priority** | Low/Med | Neither. Failover happens only by accident, between HDHR hosts whose channel ids collide [U§9.3] | Neither ("probably wont") | [R§9] |
| 10 | **IPTV extras**: Xtream Codes, catch-up, HTTP headers, `tvg-shift` | Low/Med | Plugins only; User-Agent is the only header | Referer and User-Agent headers, `tvg-shift`; Xtream through plugins | [R§10] |
| 11 | **Post-processing and conversion** | Low | Runs a script; its stdout is not logged | Script plus "Automatically Convert" (Premiere) | [R§11] |
| 12 | **Remote or "cloud" DVR** | Low | At parity. 12.0 fixed server-local stream URLs (PR #17298) | At parity | [R§12] |
| 13 | **ATSC 3.0 DRM and AC-4** | Low; cannot be solved | Neither. SiliconDust says DRM cannot be supported on Apple, Microsoft or web | Neither | [R§13] |

Jellyfin advantages to keep:

- No paywall. Emby Live TV needs Premiere, which costs $54 a year or $119
  for life.
- Legacy HDHomeRun units work. Emby needs a "DLNA 2015+" model.

### 3.2 Reading the ranking

- Gaps 1, 2 and 4 are server problems with high demand. Deadfin can fix all
  three without client work.
- Gap 3 is a client problem. For the community member it is probably the
  deciding one. SharkParty already carries the Swiftfin EPG grid; it needs
  to come out of Experimental and be tested on tvOS against a working server.
- Gap 5 (time-shift) and gap 7 (conflicts) are the clearest chances to beat
  both Jellyfin and Emby. Gap 4 needs a shared per-channel buffer anyway, and
  that buffer is the foundation for time-shift.

---

## 4. Feasibility for Deadfin

Effort: **S** is one PR, up to about a week. **M** is 2 to 4 PRs. **L** is a
multi-PR epic. **XL** is a multi-month epic that touches several subsystems.

"Stock" means that unmodified jellyfin-web and Swiftfin get the benefit
through the frozen 12.0.0 API (D18). "Client" names the repository that must
change before users see the feature.

### 4.1 The base every gap needs (the MVP, at parity)

| Component | Fit in Deadfin | Notes |
|---|---|---|
| Live TV options | New named configuration `livetv` in `internal/config`, with the `LiveTvOptions` field list [U§5]; served by the existing `/System/Configuration/{key}` | jellyfin-web reads and writes it directly |
| Tuner hosts | New package `internal/livetv/tuner`: an HDHomeRun client (HTTP JSON plus UDP discovery) and an M3U parser. Context-first; HTTP client injected; no package state | Port `M3uParser` tests (§7.1). Legacy UDP/RTP HDHR can come later |
| Guide | `internal/livetv/guide`: an XMLTV reader that streams once per refresh (upstream parses once per channel [U§9.9]) plus channel matching. A Refresh Guide task registered on `internal/tasks` (D11), every 24 h, key `RefreshGuide` | Channels and programs are items: `KindLiveTvChannel` and `KindLiveTvProgram` already exist. Program fields (start, end, episode, flags, `IsNew`) go in a satellite table (CONVENTIONS 2.2). SQLite and PostgreSQL migrations land together |
| Stream broker | `internal/livetv/stream`: one supervised reader per open channel, with consumers counted by reference. It enforces `TunerCount` across hosts, and closes on `LiveStreams/Close`, playback stop, session end and transcode kill. Implements `playback.LiveStreamProvider` | Owns goroutines per CONVENTIONS 1.3; `Close(ctx)` is goleak-tested; clock is injected for idle timers |
| Live encode | `internal/encoding`: an input that is not a file, plus the live input modifiers (`-analyzeduration`, `+igndts`, `+genpts` on copy, `-re`, reconnect flags) as pure argument builders with golden cases taken from stock ffmpeg logs (§2.2). `internal/ffmpeg`: an "infinite source" HLS job (EVENT playlist, no seek respawn, the existing inactivity kill). `internal/playback`: relax `validateHLSSource` for broker-owned live sources only | This is the largest risk. `hls_job.go` assumes `RunTimeTicks`. Keep live as its own job mode; do not add `if live` branches all over the VOD path |
| API | Replace the stubs in `internal/api/handlers/livetv.go` route by route, keeping each route's policy metadata. Add the `livetv` user view to `/UserViews`. Turn `LiveTv` ledger rows from `stubbed` into `implemented` | Each step shrinks divergence D-07 |
| Recorder | `internal/livetv/dvr`: a timer store (SQL, not upstream's whole-file JSON; the wire is unaffected), a scheduler on the injected clock, a direct TS copy from the broker (a recording is just another consumer), recording libraries through `internal/library`, an NFO writer, and an optional post-processor exec through `internal/ffmpeg`-style process control | WS `TimerCreated`, `TimerCancelled`, `SeriesTimerCreated` and `SeriesTimerCancelled` through `internal/events` |

Estimated size of the base: **XL** in total, split into three phases (§6).
Stock clients work with all of it. No client change is needed. The pinned
jellyfin-web and SharkParty already call every route in §2.3.

### 4.2 Gap by gap

| # | Gap | Deadfin mechanism | Packages | Divergence? | Effort | Stock clients? |
|---|---|---|---|---|---|---|
| 4 | Tuner handling | Covered by the broker in §4.1. Share one upstream per channel for **every** source type, including non-TS M3U through a remux to TS. Pool `TunerCount` across all hosts. When every tuner is busy, return the existing `PlaybackInfoResponse.ErrorCode` instead of a dead stream. Bound the buffer files | `livetv/stream`, `playback` | **Yes**: busy tuner gives an error and not a 200 with a dead source; M3U limits are enforced. New divergence row | in MVP (L) | Yes. Clients already handle `ErrorCode` on PlaybackInfo |
| 1 | Guide data | (a) Make XMLTV first-class: several XMLTV sources merged per channel, not first-wins; gzip; refresh XMLTV and M3U separately. (b) A Schedules Direct client that obeys SD's documented limits (token reuse, stop on 4001/4004/4005/4008, a daily image budget). (c) Point users at clean free sources (iptv-org/epg, the fakehdhr guide for tests). Emby's own guide data is proprietary and cannot be copied | `livetv/guide`, `providers` style HTTP client with cassette tests | (a) merge: yes. (b) SD rate behavior: yes, but nothing visible on the wire | M (XMLTV), M (SD) | Yes |
| 2 | Channel groups and management | Read M3U `group-title` (and HDHR source tags) into channel `Tags` and `Genres`, so the `LiveTv/Channels` and `Items` tag filters work. Add channel hide, renumber and order as server state; the stock lists then come back filtered and ordered. Admin UI through `/deadfin/v1/livetv/channels` (D13) and deadfin-web. jellyfin-web's fixed guide categories (Movies, Sports, Kids, News) come from listing-provider category lists, which the server can widen | `livetv/guide`, store, `api/handlers`, native API | Yes (hidden channels, renumbering) | M | Partly. The effect is visible in stock clients; managing it needs deadfin-web. Group filter chips need deadfin-web or SharkParty |
| 3 | Apple TV guide | Server: make `POST /LiveTv/Programs` and `GET /LiveTv/Channels?addCurrentProgram=true` fast for 1–5 k channels × 7 days (an indexed program satellite, keyset paging). Client: take SharkParty's EPG grid out of Experimental, test on tvOS, and add a mini-guide and channel-up/down in the player | store; **SharkParty** | No | S server, M client | No: needs SharkParty work |
| 5 | Time-shift | The broker keeps a per-channel **ring of HLS segments** (for example 2 h, configurable) instead of a growing `.ts`. Every viewer of a channel reads the same segment ring. A client seeks back within the window; "watch from start" for a recording in progress is the recording served as a growing EVENT playlist. AVPlayer (SharkParty) and hls.js (jellyfin-web) both support EVENT-playlist seeking | `livetv/stream`, `ffmpeg`, `hls` | Yes: segment retention, playlist window, sharing one encode across viewers | L | Mostly yes; behavior varies per client. Needs a check on Swiftfin, jellyfin-web, Android TV and Roku |
| 7 | Series rules and conflicts | Apply the fields upstream ignores: `Priority`, `KeepUntil`, `Days`, required padding. Read the XMLTV `<new/>` tag for new-only. Add a tuner-aware scheduler that predicts conflicts and sets the existing `RecordingStatus.ConflictedOk` / `ConflictedNotOk` on `TimerInfoDto`. Keyword rules (Emby 4.7.7) through the native API | `livetv/dvr` | Yes (fields applied; conflict statuses emitted) | M (apply fields), M (conflicts), S–M (keyword) | Mostly yes. The fields and statuses are on the wire; whether each client displays conflicts must be checked. Keyword rules need deadfin-web |
| 6 | Commercial skip | After a recording: run comskip (an external binary, GPL, never linked) → EDL → media segments of type `Commercial`. **Prerequisite: Deadfin does not yet serve `/MediaSegments/{itemId}` (ledger: deferred).** jellyfin-web 12.0 already has segment actions ("Ask to skip" / skip) | `livetv/dvr` post-step, `mediasegments`, store | Yes (an in-tree segment provider that upstream does not have) | M (+ M for MediaSegments if not done first) | jellyfin-web yes; Swiftfin/SharkParty need a check |
| 8 | Logos and mapping | A per-tuner logo-source option (M3U or guide). Re-fetch the image on remap. A "None" mapping | `livetv/guide`, `images` | Small: a new option field, plus a native or admin UI | S | Partly; the option needs deadfin-web |
| 9 | Failover | Channel identity keyed by guide id across hosts. On an open failure or a stall mid-stream, the broker reopens from the next source in priority order. The consumers' segment ring continues | `livetv/stream` | Yes | M (after time-shift: S) | Yes |
| 10 | IPTV extras | Per-tuner request headers (Referer, User-Agent, `#EXTVLCOPT`), passed to the broker *and* the recorder (upstream drops them in `EncodedRecorder` [U§9.12]); `tvg-shift`; an Xtream Codes tuner type (credentials in config, never in URLs or logs); catch-up later | `livetv/tuner` | Yes (new tuner type) | S (headers), M (Xtream) | Yes for playback; setup needs deadfin-web for new tuner types |
| 11 | Post-processing | Parity: a script with `{path}`, plus logging its stdout. Optional native remux to MKV or MP4 with the existing ffmpeg process control | `livetv/dvr` | Small | S | Yes |
| 12 | Remote DVR | Parity once the broker URLs are relative or proxied. On k8s, recordings go to a PVC (NFS RWX, like media) | deploy | No | S | Yes |
| 13 | ATSC 3.0 | Pass DRM flags through (`lineup.json` `DRM: 1` → hide or mark the channel). Probe AC-4 support in the bundled ffmpeg and report it in capabilities | `livetv/tuner`, `ffmpeg` | No | S | Yes |

### 4.3 Credentials in source URLs (Xtream panels)

Xtream Codes panels are the most common paid IPTV source. Upstream has no
Xtream client: users add `get.php?username=U&password=P&type=m3u_plus`
as an M3U tuner and `xmltv.php?username=U&password=P` as an XMLTV
provider. Parity is therefore the M3U and XMLTV path, not a new tuner type;
an Xtream tuner type stays in 5g. Every URL carries the account
credentials: the two configured URLs in the query, and every channel URL
in the path (`/live/U/P/<id>.ts`).

**Measured on stock 12.0.0** (`tools/fakehdhr/xtream-probe.sh`, synthetic
panel, a marker password; §5.3):

| Surface | Stock exposes the password | Deadfin rule |
|---|---|---|
| `POST /LiveTv/TunerHosts`, `POST /LiveTv/ListingProviders` responses; `GET /System/Configuration/livetv` (admin) | Yes: the raw `Url`/`Path` | **Parity, admin only.** jellyfin-web's edit forms post the whole object back, so a masked value would overwrite the stored one. Keep the raw value for administrators |
| `PlaybackInfo` `MediaSources[].Path` (**any user**) | **Yes**, when the source is not shared: an `.m3u8` channel URL, or `AllowStreamSharing` off. With TS and sharing, `Path` is `/LiveTv/LiveStreamFiles/<id>/stream.ts` | **Divergence (approved by the operator, 2026-10-13).** `Path`, and every other response field that would carry a raw tuner or provider URL, is always the broker URL (`/LiveTv/LiveStreamFiles/...`), never the upstream URL, for every user. The phase 2 broker owns every source, so no client needs the raw URL |
| Server log | **Yes**, at `Information`: `xmltv path: <url>` **once per channel per refresh**, `Downloading xmltv listings from <url>`, `Opening SharedHttpStream Live stream from <url>`, and `Live stream opened: {MediaSource}` with the raw `Path` | Redact. Never log a configured or derived source URL raw |
| ffmpeg command line in the transcode log | Yes for non-shared sources (`-i <url>`) | ffmpeg reads the broker URL. If ffmpeg must read the source itself, the logged command line is redacted |
| Disk, besides the stored config | Yes: the channel cache `/cache/<id>_channels` and the probe cache `/cache/mediainfo/<id>.json` | Store derived channel URLs only in the database next to the tuner config, never in caches. Probe results are keyed by channel id and hold no URL |
| Channel lists, items, programmes, sessions, master playlists (viewer) | No | Parity |

**Mechanism.** One pure package, `internal/livetv/secret` (or a helper in
`internal/livetv/tuner`):

- `Redact(u string, secrets []string) string` masks:
  - the user info;
  - the values of query keys that name a secret (`password`, `pass`,
    `pwd`, `token`, `key`, `auth`, `signature`, `username`);
  - every path segment or substring equal to a known secret value.
- `Secrets(tuner)` collects the secret values from the configured URLs:
  the query values, the user info, and the Xtream `/live/U/P/` path pair.
  Channel URLs that come from that tuner are then masked **by value**.
  That catches `/live/U/P/<id>.ts`, which no key-based rule would see.
- All tuner and listing log lines, error messages wrapped with `%w`, and
  the ffmpeg command-line logger pass through it. A
  `convention-gates` rule or a vet check forbids logging a source URL
  variable without it, where that is mechanical.

**Tests** (draft 1 and draft 2 acceptance): a synthetic URL with a marker
password, through the whole path (add tuner, refresh, open, play, error,
close), with the log captured. Assert:

- the marker appears in no log line, in no error, and in no API response
  except the admin configuration routes above;
- the marker is in no file under the cache and transcode paths;
- `PlaybackInfo` `Path` is the broker URL for every source type.

**Do not hammer a provider.** IPTV panels ban client IPs for crawling,
failed logins and probing. A ban cuts off the whole household, and a
retry loop is enough to cause one. The tuner and guide code follows these
rules:

- **401 means stop.** A 401 (or an Xtream "auth: 0") on the M3U, XMLTV or a
  stream puts the source into a **"credentials rejected"** state, visible
  in the tuner status and in a clear playback error. Deadfin sends no more
  requests to that source until an administrator edits it or presses
  retry. It never loops on bad credentials.
- **403 and 429 back off.** Retry with exponential backoff and jitter,
  starting at 1 min and capped at 1 h, and honor `Retry-After`. Several
  403s in a row are treated as a possible ban: the source stops and is
  marked "blocked by provider".
- **The expired-URL re-read (§0.1) is bounded.** It is single-flight per
  source, at most once per 5 minutes per tuner, and only on 403, 404 or
  410 from a *stream* URL, never on a 401.
- **Refreshes are rate-limited.** The M3U and XMLTV of one source are each
  fetched at most once per refresh, and at most once per 15 minutes,
  whatever triggers them. Stock downloads the XMLTV once per refresh and
  caches it for an hour; Deadfin keeps that.
- **Concurrent opens per tuner are capped.** `TunerCount` limits the
  connections per source (the Xtream `max_connections`; stock has the same
  per-tuner limit). Probing reuses the tune connection and never opens a
  second one. Twenty viewers on one channel make one upstream connection.
- **No probing sweeps.** Deadfin never opens channels to check them in
  bulk, and never calls `player_api.php` or other undocumented endpoints.

These rules apply to every source, but they are tested against the fake
panel (`-xtream-ban-after`, L16), never against a real provider.

**An Xtream tuner type (beyond parity).** Providers treat a full
`get.php` dump as crawling, and some ban for it. The polite path is the
JSON API. It is not parity: upstream has no Xtream client.

- **Channels:**
  - one `player_api.php?action=get_live_categories`;
  - then `action=get_live_streams`: once for all categories, or once per
    enabled category (`&category_id=N`) when only a few are enabled;
  - live only, never the VOD or series actions.
  
  The administrator picks categories, and the tuner fetches only those.
- **Stream URLs:** `<base>/live/<user>/<pass>/<stream_id>.ts` (or `.m3u8`
  when `allowed_output_formats` asks for it). These URLs are built, never
  logged: the §4.3 redaction masks the `/live/U/P/` segments by value.
- **Guide:** **only** the bulk `xmltv.php?username=&password=`, through
  the parity XMLTV listings path. Channels match by `epg_channel_id`
  against XMLTV `<channel id>`, then by name. It refreshes on the guide
  schedule, at most every few hours, with the backoff above. **Never call
  `get_short_epg` or `get_simple_data_table`.** Per channel, they hit
  provider rate limits.
- **Account:** the plain `player_api.php` call answers `user_info`. The
  tuner reads `auth`, `status`, `exp_date` and `max_connections`, which
  is the default `TunerCount`. **That response echoes the password**, so
  its body is never logged or cached.
- **Field shapes vary by panel.** Ids come as strings in some panels and
  as numbers in others; `epg_channel_id` and `stream_icon` may be empty or
  null. The decoder accepts both forms (fakehdhr quirk `xtream-types`).
  The shapes follow the public notes in Fermata discussion #434.

Two ways to place it. D6 rules out a plugin loader, so a "plugin" here
means an in-tree seam (`docs/07-extension-seams.md`):

| Option | What it is | Cost |
|---|---|---|
| A. A built-in tuner type `xtream` | `TunerHosts/Types` lists `xtream`; `TunerHostInfo` keeps its wire shape (`Url` = panel base; credentials in their own fields, or parsed from a pasted `get.php` URL) | A divergence row (new type value). Setup and category picking need deadfin-web; whether stock jellyfin-web's tuner form handles an unknown type is not checked **[U]** |
| B. A tuner seam only | `internal/livetv/tuner` defines `Host` (list channels, open stream, status) with a registry composed in `cmd/deadfin`. Xtream ships later, or out of tree as ordinary Go code | No divergence now. Users keep the `get.php`-as-M3U path, which is the crawl-like path providers dislike |

**Recommendation: build B in phase 1 and A on it as phase 1b.** The `Host`
seam is needed anyway: M3U and HDHomeRun are its first two
implementations. Xtream as a third, in-tree implementation is small once
the M3U model exists. It gives the operator's test setup a polite
channel source early. The M3U path stays at exact parity, with no
auto-switching of `get.php` URLs behind the user's back. Add one
divergence row: "`TunerHosts/Types` adds `xtream`; live channels from
`player_api.php`; guide from `xmltv.php`."

**User-Agent.** Some providers block an empty User-Agent and the defaults
of ffmpeg (`Lavf/...`) and Go (`Go-http-client/...`). fakehdhr records the
User-Agent of every provider-side request (`/stats.json` `UserAgents`),
and `-block-default-ua` answers 403 to those defaults. On stock 12.0.0
(`xtream-probe.sh`):

| Request | Stock User-Agent | Source |
|---|---|---|
| M3U fetch (`get.php`) | `Jellyfin-Server/12.0.0`, or the tuner's `UserAgent` when set | `M3uParser.cs:58-60`; the default named client (`Startup.cs:100-102`) |
| XMLTV fetch (`xmltv.php`) | `Jellyfin-Server/12.0.0` (no per-provider override) | default named client |
| TS stream through `SharedHttpStream` | `Jellyfin-Server/12.0.0` | default named client |
| Stream read by ffmpeg (`.m3u8`, unshared) | the tuner's `UserAgent`, or else a fixed Chrome 122 string, passed with `-user_agent` | `M3UTunerHost.cs:162-166`; `EncodingHelper.GetUserAgentParam` |

Stock never sent `Lavf`. The Deadfin rule:

- One application User-Agent, `Deadfin/<version>`, on every provider
  request made by Go code: M3U, XMLTV, `player_api.php` and broker stream
  reads.
- The tuner's `TunerHostInfo.UserAgent`, when set, replaces it on that
  tuner's requests, as upstream does for M3U fetches and streams. For
  Xtream, it also covers `xmltv.php`.
- ffmpeg always gets `-user_agent` for any HTTP input, including probes,
  so it never sends `Lavf`. The value is the tuner override if set.
  Otherwise it is upstream's browser string, for parity on unshared
  streams. Where the broker reads the stream (the phase 2 default), it is
  `Deadfin/<version>`.
- Tests: an `httptest` provider asserts the User-Agent on each fetch kind,
  with and without an override. A golden case asserts `-user_agent` in
  every live ffmpeg argv. An L16 run with `-block-default-ua` sees no 403.

**Handling rule for a real account.** A community member lends the
operator an Xtream account for manual testing. Only the operator uses it,
on their own server, through the normal Live TV settings or the §5.7 hook.
Nothing from that service enters the repository, PRs, issues or comments:
no host name, credentials, responses, channel names or guide data. The
provider bans IPs for crawling, failed logins and probing, so no session,
tool, script or test ever contacts it; only the operator does, by hand.
Fixtures are synthetic (`fakehdhr` `-xtream-*`).

**Scale.** Real Xtream `m3u_plus` playlists hold tens of thousands of
entries, with live channels, movies and series episodes mixed, plus a
large XMLTV file. Upstream imports every entry as a channel, VOD included
(`M3uParser` filters only on the URL scheme). Deadfin adds a per-tuner
option to skip entries whose path is `/movie/` or `/series/`, or whose
extension is not a live container. The option defaults to parity (import
all), and it gets its own divergence row. L13 (§5.2) measures the import.

### 4.4 Multi-source channel merging

The operator will test two, soon three, Xtream providers whose channel
lists overlap. Without merging, every channel appears once per provider,
with different names and different guides.

**Parity default.** Upstream keeps each tuner's channels separate:
duplicates appear once per tuner. Guide matching is by id, plus manual
`ChannelMappings` on the listings provider. Deadfin keeps exactly that
while merging is off, and merging is off by default.

**The extension (Emby and Channels DVR style).** One **logical channel**
is backed by N sources, ordered for failover.

- **Normalize names** before matching:
  - strip provider prefixes (`US|`, `UK:`, `[VIP]`, repeated);
  - strip quality suffixes (`HD`, `FHD`, `UHD`, `4K`, `HEVC`, `SD`);
  - strip punctuation, fold case and collapse spaces.
  
  Timeshift markers (`+1`, `+2`, "plus 1") are kept. A timeshift channel is
  a **distinct** logical channel, linked to its parent as a sibling.
- **Match in stages, each with a confidence:**

  | Stage | Rule | Confidence | Auto-merge |
  |---|---|---|---|
  | 1 | The same guide id (`tvg-id` / `epg_channel_id`) in two providers | 1.0 | yes |
  | 2 | The same normalized name, where that name is unique within each provider | 0.9 | yes |
  | 3 | The same normalized name and the **same guide schedule**: at least 80 % of the next 24 h of programme titles and start times agree. This settles ambiguous names: "X FHD" and "X 4K" in one provider are one channel; "FAKE SD" and "FAKE HEVC" are two | 0.95 | yes |
  | 4 | A call-sign name (`WABC-DT`) against call signs in other guides' `<display-name>` | 0.85 | yes |
  | 5 | Fuzzy: a unique best token-set Jaccard of 0.75 or more, never across a timeshift marker | the score | **no**: offered as a suggestion in deadfin-web |

- **Manual overrides** win over every stage and survive refreshes:
  - *merge* two sources;
  - *split* a source out of a logical channel;
  - *pin* a preferred source;
  - *exclude* a source.
  
  An override names a source by a **stable source key**: the tuner id plus
  the provider's stream id (Xtream `stream_id`), or else the `tvg-id`, or
  else the normalized name. Never the URL: URLs carry credentials and
  tokens, and both change.
- **Failover order** for a logical channel:
  1. the pinned source;
  2. then provider preference (an ordered tuner list);
  3. then quality preference (configurable; for example HD before 4K on
     bandwidth-limited clients).
  
  On an open failure, the broker tries the next source. Failures include
  connect errors, timeouts, 403, 404 and 5xx, a tuner-limit hit (805 or
  Xtream `max_connections`), and a geo block. A failed source cools down
  under the §4.3 backoff, and a 401 takes the whole tuner out (§4.3).
  Connection limits count per provider, across all logical channels.
  Mid-stream failover is the 5g broker behavior, with the segment ring
  kept running.
- **Guide.** Each provider brings its own XMLTV, with its own channel ids.
  Each logical channel takes **one canonical guide**: the first source,
  in provider preference order, whose guide covers at least 90 % of the
  next 48 h. Programme lists are **never merged** across guides. The
  choice is sticky, and changes only when coverage stays low for two
  refreshes. That keeps programme ids, and the single timers that point at
  them, stable. When the guide does change, single timers are re-keyed by
  (logical channel, start, title).
- **Storage** in `internal/store`, with SQLite and PostgreSQL migrations
  together:
  - `livetv_logical_channel`: the id, number, name and canonical guide
    source;
  - `livetv_channel_source`: the logical id, tuner id, source key, rank,
    matching stage and confidence, last success, and cool-down;
  - `livetv_merge_override`: the kind, source key, logical id, and who
    made it and when.
  
  The logical channel's item id derives from its stored id, not from any
  source. Favorites, user data, recordings and series timers therefore
  survive sources that come and go. When merging is turned on, the user
  data of the preferred source's existing channel item moves to the
  logical item once.
- **Divergence, not a plugin seam.** Merging changes what `/LiveTv/Channels`
  and `/LiveTv/Programs` return, the channel model, the store, and the
  broker's open path. A tuner implementation behind the `Host` seam (§4.3)
  cannot do that, because the seam sits below the channel model. So
  merging is core code behind an option (off by default = parity), with
  one divergence row: "Live TV channel merging: one logical channel per
  matched set of tuner channels, with ordered failover." Managing
  overrides needs deadfin-web, through the native API (D13). Stock clients
  only see the merged list.

**Prototype result.** A test-only prototype matcher (`tools/fakehdhr`
`persona_test.go`) runs over the three synthetic providers (§5.1
personas, 1 800 source entries):

- with the guide-schedule stage: pairwise precision 1.00 and recall 1.00;
- with names only: precision 1.00 and recall 0.88. The misses are exactly
  the ambiguous names.

The synthetic schedules match exactly, so 1.00 is an upper bound. On real
guides, the 80 % rule is the parameter to tune, against the operator's
local check. The fuzzy stage did not fire on these fixtures; a persona
with abbreviations and typos is the next fixture to add.

### 4.5 Risks

- **`internal/ffmpeg` job model.** Live is the first source with no runtime,
  no seek and several consumers per encode. Plan a design pass on
  `hls_job.go` and `transcode_manager.go` before the first live PR.
- **The anonymous stream routes** (`LiveStreamFiles`, `LiveRecordings`) are
  upstream parity [U§9.14]. Deadfin can keep them anonymous but use
  unguessable ids that expire with the stream. The security-audit session
  should review this.
- **Clock-driven code.** Guide windows, timers and padding must run on
  `clock.Clock`. The DVR scheduler is the first long-horizon timer user and
  needs a fake-clock test suite.
- **Disk.** Buffers, rings and recordings need hard limits and a
  free-space check. Upstream's known failure is a full transcode directory
  (#17120).

---

## 5. Testing: a synthetic limits harness first

The goal of testing is to exercise the code and find where it breaks, not to
collect sources. The synthetic side is therefore the centre of the plan:

- `tools/fakehdhr` generates lineups, guides and streams of any size, with
  the real-world mess and faults switched on as needed. CI and every
  acceptance budget rely on it.
- Real public sources (iptv-org, FAST, vendor test streams) are a sanity
  check that the synthetic model matches reality (§5.6).
- Operators may test against any M3U or XMLTV source locally (§5.7). Such
  sources never enter the repository.

### 5.1 The harness: `tools/fakehdhr`

A stdlib Go command in the root module, package `main`, the same layout as
`tools/perfcompare`. It is a prototype, not part of the server.
SiliconDust publishes the HDHomeRun HTTP API it emulates
(https://info.hdhomerun.com/info/http_api). Plex, Channels DVR, Emby and
Jellyfin all integrate against that API, so it is the de facto tuner
contract.

| Dimension | Knob | What it produces |
|---|---|---|
| Tuner API | always on | `/discover.json`, `/lineup.json`, `/lineup_status.json`, `/status.json`, `/auto/v<GuideNumber>`; UDP discovery on 65001, with the CRC checked against upstream's request (end-to-end discovery needs `-listen :80`); `ModelNumber` "HDTC" enables upstream's transcode profiles; `-drm` returns 403/`807`; an unknown channel returns 404/`804` |
| Tuner limit | `-tuners N` (`0` = unlimited) | A tune past N gets 503 with `X-HDHomeRun-Error: 805 All Tuners In Use` |
| Lineup scale | `-channels N` | N generated channels (`1000.1` upward) besides the 6 base and 9 fault channels. The cheap "lite" profile is used; every 20th channel has no guide data |
| Source shapes | per channel | Interlaced MPEG-2 1080i/AC-3 5.1 (ATSC 1.0), 480i SD, H.264/AAC, HEVC 1080p60/AC-3, lite H.264 240p. `-loop capture.ts` replays a capture instead |
| M3U | `/playlist.m3u`, `/lineup.m3u` (the HDHomeRun name) | The same lineup, with `tvg-id`, `tvg-name`, `tvg-logo`, `tvg-chno`, `group-title` and `url-tvg` |
| Guide size | `-guide-days D`, `/xmltv.xml`, `/xmltv.xml.gz` | Streamed in constant memory. Contiguous programmes, series with `xmltv_ns` and `onscreen` numbers, `<new/>` and `<previously-shown/>`, upstream's category names, logos. `-guide-anchor` fixes the start time for deterministic runs |
| Guide and playlist quirks | `-quirks all` or a list | `missing-ids`: every 10th M3U entry has no `tvg-id`. `dupes`: every 50th channel appears twice in M3U and XMLTV, with duplicated programmes. `tz`: per-channel offsets `+0000`, `-0400/-0500` (New York, including the DST changeover), `+0530`, `+1245/+1345`. `overlap`: programmes that start before the previous one stops. `gaps`: holes. `no-stop`: no `stop` attribute. `entities`: `&`, quotes, `<>`, 300-character titles, non-ASCII and emoji. `reverse`: reverse time order within a channel |
| Stream faults | channels `900.1`–`900.9`, or `?fault=` on any tune | `slowstart=8s` (headers at once, bytes late), `hang` (no bytes, ever), `stall=3s/20s`, `die=30s`, `status=503`, and alternations every `every=`: `codec=atsc1\|h264`, `bitrate=1M\|25M`, `audio=2\|6`, `size=1280x720\|1920x1080`. `-drop-after 30s` ends every stream, for signal loss. Each phase is a fresh encoder run, so timestamps and continuity counters restart, as at a real broadcast splice. A codec change on the same PID breaks naive decoders, which is the point |
| **FAST-style HLS** | channels `800.1`–`800.8` (M3U only, not in the HDHomeRun lineup); `-fast-channels N` for scale; `-geo-allow CIDR` | Live HLS at `/hls/<n>/master.m3u8`: a sliding 6-segment window, `EXT-X-PROGRAM-DATE-TIME`, and a correct `EXT-X-DISCONTINUITY-SEQUENCE`. `800.2` SSAI: a 32 s ad break every 120 s, with `EXT-X-DISCONTINUITY` and `EXT-X-CUE-OUT`/`-CONT`/`-IN`; the ad creative is 1080p30 with 5.1 AAC inside 720p29.97 stereo content, and has its own timestamps. `800.3`: the same with `EXT-X-DATERANGE` `SCTE35-OUT`/`-IN`. `800.4`: HMAC tokens that expire 120 s after the M3U fetch (403 after), with a fresh token on each M3U fetch. `800.5`: geo 403 unless the client is in `-geo-allow`. `800.6`: an unmarked timestamp jump at the loop point. `800.7`: a playlist that stops advancing for 20 s every 120 s. `800.8`: all of these together. Segments are rendered once per shape with ffmpeg and served from memory |
| **Xtream-style panel** | `-xtream-user`, `-xtream-pass` (default marker `FAKEPASS-7b1e`), `-xtream-movies N`, `-xtream-episodes N`, `-guide-share F`, `-xtream-ban-after N`, `-xtream-ban-for D` | `get.php?username=&password=&type=m3u_plus&output=ts\|m3u8` mixes live channels (`/live/U/P/<id>.ts`) with VOD (`/movie/U/P/<id>.mp4`) and series (`/series/U/P/<id>.mkv`), with `tvg-*` and `group-title`. `xmltv.php` serves the guide, covering a fraction `F` of the generated channels. `player_api.php` gives the account check. Wrong credentials get 401. After N failed logins inside D, the client IP gets 403 "banned" for D, as real panels do. `/stats.json` counts `XtreamRequests`, `AuthRejects` and `BanRejects`. fakehdhr never logs the credentials (tested) |
| **Provider personas** | `-persona a\|b\|c` (run one instance per persona), `/truth.json` | The same channels as three providers list them. `a`: `US\| X HD` with canonical ids. `b`: `[VIP] X` or `X FHD` with its own ids (`FakeNews.us`); drops 1 in 10; adds `+1` timeshift channels (programmes shifted 1 h) and `4K` copies. `c`: `UK: X`, or the call sign `WABC-DT` for 1 in 5; shares `a`'s ids for 1 in 3, its own for 1 in 3, none for the rest; drops 1 in 10; `+1` channels overlapping `b`'s. Every XMLTV `<display-name>` list carries the call sign. `/truth.json` gives each channel's canonical identity for scoring |
| WAN tuner | `?fault=cap=8M,latency=150ms` on any tune | A throughput cap below the stream rate (a remote tuner's uplink) and a delay before the response. Packet loss and jitter are below HTTP: put `tc netem` or toxiproxy in front |
| Concurrency | `-source cache` | Renders a 10 s clip per channel shape once, then replays it from memory, paced to its bitrate. No ffmpeg runs per viewer. The loop point is a timestamp discontinuity |
| Tuner-side truth | `/status.json`, `/stats.json` | Active and peak streams, opens, 805 rejections, faulted opens, bytes sent. The server under test is checked against these counts |

Tests (`go test -race ./tools/fakehdhr`) need neither ffmpeg nor sleeps. Time
runs through an injected pacer. They cover:

- the discovery frame;
- the lineup, the tuner limit and release, and unlimited tuners;
- DRM, invalid-channel, status and hang faults;
- fault parsing and phases;
- slow start and stalls on a fake clock;
- clip pacing;
- guide continuity, and every quirk, including both DST offsets;
- 2 000-channel guide streaming with flat heap;
- gzip output;
- the ffmpeg arguments per profile and phase.

Scripts:

- `tools/fakehdhr/stock-smoke.sh`: the §2.2 end-to-end run against stock
  12.0.0.
- `tools/fakehdhr/guide-limits.sh`: one guide-import limits run, reported as
  one JSON line (§5.2, L1; results in §5.3). `CHANNELS`, `DAYS`, `QUIRKS` and `TARGET` select the
  run.
- `tools/fakehdhr/dev-sources.sh`: the local private-source loader (§5.7).

```bash
go run ./tools/fakehdhr -channels 20000 -guide-days 14 -quirks all -tuners 0 -source cache
```

```bash
CHANNELS=1000 DAYS=7 QUIRKS=all tools/fakehdhr/guide-limits.sh
```

### 5.2 Scenarios and the metrics to watch

| # | Scenario | Harness setting | Deadfin metrics | How measured |
|---|---|---|---|---|
| L1 | **Guide import at scale** | `-channels` 100 / 1 k / 5 k / 20 k × `-guide-days` 7 / 14, `-quirks` none and all | guide import wall time; programmes per second; peak RSS and heap; DB size growth (db + WAL); **writer-lock hold time** (one SQLite writer, CONVENTIONS 4.5: a long refresh must not block playstate writes) | `/ScheduledTasks` `LastExecutionResult`; pprof heap and goroutine (`cmd/deadfin/pprof.go`); file sizes; a concurrent `POST /Sessions/Playing/Progress` latency probe during the refresh |
| L2 | **Guide grid queries** | after L1 | p50/p95 of `POST /LiveTv/Programs` (100 channels × 6 h, as jellyfin-web's guide sends), `GET /LiveTv/Channels?addCurrentProgram=true&limit=100`, `GET /LiveTv/Programs/Recommended`; query plans | client timing in the driver (§5.4); `EXPLAIN QUERY PLAN` in store tests; storetest on both backends |
| L3 | **Quirk correctness** | `-quirks all`, `-guide-anchor` before a DST change | channels matched versus expected; one channel per duplicate; programmes in the repeated DST hour kept and correctly ordered; overlaps and missing stops handled as upstream does; no import failure | counts compared with stock under the same anchor (difftool) |
| L4 | **Stream open** | normal, `900.1` slow start, `900.8` hang, `900.9` dead, tuner limit reached | PlaybackInfo → first HLS segment latency; time until a clear error on hang and dead; error shape on busy tuner; **tuner released** after each failure | driver timing; fakehdhr `/stats.json` `ActiveStreams` back to 0 |
| L5 | **Mid-stream faults** | `900.2` stall, `900.3` die, `900.4` codec, `900.5` bitrate, `900.6` audio, `900.7` resolution | client-visible stall (the largest gap between segment arrivals); recovery or clean end; ffmpeg restarts; **ffmpeg process count**; orphan processes; transcode directory size | driver segment timing; process-group count; directory size over time |
| L6 | **Failover** (5g) | two fakehdhr instances with the same lineup; primary on `die=60s` | **failover time** (last segment before to first after); viewer continuity; the playlist stays one EVENT playlist | driver timing; both instances' `/stats.json` |
| L7 | **Concurrency** | `-source cache -tuners 0`: (a) 100 viewers on one channel; (b) 200 viewers on 50 channels with `-tuners 8`; (c) 20 recordings plus 50 viewers | (a) tuner opens = 1 (sharing), ffmpeg count per channel/profile; (b) exactly the expected 805s turned into PlaybackInfo errors; (c) every recording complete, size ≈ bitrate × duration, no gaps; CPU, RSS, goroutines, file descriptors | fakehdhr `/stats.json`; process count; pprof; recording ffprobe |
| L9 | **FAST ingest across ad breaks** | `800.2`, `800.3`, `800.6`, `800.8`; 1 and 20 viewers | client-visible stall at each break; the output playlist's discontinuity tags and sequence (correct, or normalized away); output shape constant when normalizing; remux versus normalize CPU per channel; **upstream fetches per channel** (1 with the broker, N without) | driver segment timing; ffprobe of the output across a break; fakehdhr `HLSSegments` divided by the segment rate |
| L10 | **Expiring URLs** | `800.4`; tune at +130 s; an expiry mid-stream | time from the 403 to playback again; **M3U re-fetches** (1 per expiry, single-flight across viewers); no tuner or ffmpeg left over from the failed attempt | driver timing; fakehdhr `TokenRejects` and M3U request count |
| L11 | **Geo block and stale playlist** | `800.5`; `800.7` | time to a clear `PlaybackInfo` error; no retry storm (geo); no false "stream died" on a stale window shorter than the timeout | driver; fakehdhr `GeoBlocks` |
| L12 | **Remote tuner over WAN** | `?fault=cap=` at 0.5×, 1× and 2× the stream rate; `latency=150ms`; netem loss of 0.5 % and 2 % | open latency; a clear "link too slow" at 0.5×; at 2× with loss, recovery without a tuner leak; ATSC 1.0 deinterlaced output on QSV | driver; `/stats.json`; ffprobe `field_order` of the output |
| L13 | **Xtream-scale import** | `XTREAM=1`, 10 k live + 15 k movies + 5 k episodes (30 k entries, 5.6 MB M3U), guide for 35 % of live × 2 days, `-quirks all` (98 MB XMLTV) | M3U parse time and peak memory; channel sync time (entries to channel items); XMLTV parse time and peak memory (**streaming: flat heap**); DB growth; VOD entries skipped or imported per option | `guide-limits.sh`; pprof heap during the refresh; a Go benchmark of the parsers with `-benchmem` |
| L14 | **Credential exposure** | `-xtream-pass` marker; `output=ts` and `output=m3u8`; tuner count 0 and 4 | occurrences of the marker in logs, errors, API responses (admin and viewer), cache and transcode files: 0 outside the admin config routes | `xtream-probe.sh` with `TARGET=<deadfin image>`; the Go tests of draft 1 and draft 2 |
| L15 | **Long-running TS** | one TS channel open 12 h through `-source cache`; a timeshift window once 5a exists | buffer file or ring size over time (bounded); RSS flat; no timestamp-wrap failure (the PTS wraps every 26.5 h, the cached clip loops every 10 s); output still in sync at the end | file sizes per hour; ffprobe of the last segments |
| L16 | **Provider politeness** | Xtream panel with `-xtream-ban-after 5 -xtream-ban-for 10m -block-default-ua`; (a) a wrong password; (b) the password changed while 20 viewers watch; (c) 50 viewers opening 10 channels at once with `TunerCount` 4; (d) a forced 403 storm; (e) the Xtream tuner type through a full refresh | provider requests per hour under failure (`XtreamRequests`); **`BanRejects` = 0** (the fake never bans Deadfin); time to the "credentials rejected" state (one attempt); 0 requests after it until an admin retries; concurrent upstream connections ≤ `TunerCount`; M3U and XMLTV fetches per refresh = 1 each; (e) `player_api` calls per refresh ≤ 1 + enabled categories, **`JSONEPGCalls` = 0**, no VOD or series actions; no request with an empty, `Lavf` or Go default User-Agent (`UserAgents`) | fakehdhr `/stats.json`; tuner status API; driver |
| L17 | **Multi-source merging** | three instances, `-persona a`, `b`, `c` (Xtream panels), 600 generated channels each; faults: `?fault=status=503` on `a`'s copy of a channel, `-tuners 2` on `b`; then restart `b` with 20 channels renamed | pairwise precision and recall against `/truth.json` (targets ≥ 0.99 and ≥ 0.97); auto-merges per stage and suggestions; failover order (`a` fails → `b` → `c`), with the time per step; a tuner-limit hit moves to the next source; overrides and logical ids survive 3 refreshes and the rename; one canonical guide per logical channel, never merged programmes | matcher report; driver; each instance's `/stats.json`; store queries |
| L8 | **Soak** | 24 h, `die=30m` on a third of channels, `800.8` on a tenth, daily guide refresh | leaks: goroutines, file descriptors, RSS, transcode and buffer directory size, ffmpeg count back to 0 when idle | pprof snapshots each hour; `/proc` counts |

**How Deadfin exposes these.** Today Deadfin has only pprof
(`cmd/deadfin/pprof.go`); the rest is read from outside. The harness issue
(§6.2, draft 0) adds a read-only Live TV counters snapshot on the pprof
listener (expvar style; operator-only; no change to the parity API):

- open broker streams, consumers per stream, and tuner usage per host;
- live ffmpeg jobs;
- buffer bytes;
- the last guide refresh's duration, channel count and programme count;
- writer-lock wait time.

**Budgets.** The first run against stock 12.0.0 sets the reference for each
row. Deadfin's acceptance budget is "no worse than stock" where stock can
complete the run. Where stock fails or has no equivalent (L6, L7a), the
issue sets an absolute budget.

### 5.3 Measured so far (2026-10-12, Apple M-series laptop)

Harness side:

| Run | Result |
|---|---|
| XMLTV, 20 000 channels × 14 days, all quirks | 3.88 GB (132 MB gzip), streamed in 24 s (gzip 33 s); fakehdhr RSS ≈ 36 MB |
| M3U, 20 015 entries | 3.8 MB in 18 ms |
| 200 concurrent tunes, `-source cache` | 200 × 4.4 Mbps (≈ 880 Mbps total), fakehdhr 4.6 % CPU, 32 MB RSS, **0 ffmpeg processes** |
| `die=4s` | connection closed at 4.0 s |
| `900.1` slow start (8 s) | headers at 0.4 ms, no bytes within a 6 s client timeout |
| `codec=atsc1\|h264,every=3s` | the capture probes as MPEG-2 and then throws H.264 decode errors after the splice, as intended |

Stock Jellyfin 12.0.0 baseline (`guide-limits.sh`):

| Run (`guide-limits.sh`) | Import | Peak memory | DB | Channels / programmes | 100 ch + current | Grid 100 ch × 6 h |
|---|---|---|---|---|---|---|
| 1 000 ch × 7 d, `-quirks all` (98 MB XMLTV) | **950 s** | 639 MiB | **775 MB** | 1 026 / 185 472 | 63 ms | 193 ms |

The run completed. These findings set the phase 1 budgets:

- **Import time is channels × file size.** Stock re-reads and re-parses the
  **whole** XMLTV file once per channel
  (`XmlTvListingsProvider.GetProgramsAsync` opens a new `XmlTvReader` for
  each `channelId`). The log shows one `xmltv path:` line per channel, at
  about 0.9 s each for 98 MB. At 20 k × 14 days (3.9 GB) that is
  impractical: about 20 000 parses of a 3.9 GB file. Deadfin's importer
  parses once per refresh (draft 1), so its L1 cost must grow with the
  file size, not with channels × size.
- **About 4.3 KB of database per programme.** 185 k programmes took
  775 MB. A 5 k × 14 day guide would be several GB in stock. Deadfin's
  program satellite (draft 1) should stay well below that; the budget is
  set from the draft 0 run.
- The 1 026 channels are the 1 000 generated plus the 20 `dupes` copies
  and the 6 base channels; stock kept the duplicates as separate channels.

Stock against the synthetic Xtream panel at scale (L13: `XTREAM=1
CHANNELS=10000 GUIDE_SHARE=0.35 DAYS=2 MOVIES=15000 EPISODES=5000
QUIRKS=all TIMEBOX=600 guide-limits.sh`). The playlist had 30 006
`m3u_plus` entries (5.6 MB) and the XMLTV was 100 MB:

- **Every entry became a channel**, including the 20 000 VOD and series
  entries: `channels_seen` was 30 006 after 10 minutes.
- **The guide refresh would take about 6 hours.** In 600 s stock finished
  840 per-channel XMLTV parses (about 0.7 s each), out of 30 006. The task
  was cancelled at 17.4 % progress.
- Peak memory was 772 MiB, and the database was 269 MB at that point, with
  43 635 programmes stored.
- Grid queries were 69 ms for both the 100-channel current and the 6 h
  grid, on a partial guide.

The fakehdhr side served the same 30 k playlist in 25 ms and the 98 MB
guide in 0.6 s, at 42 MB RSS.

Credential exposure on stock (L14: `xtream-probe.sh`, a marker password):
see the table in §4.3. In short:

- a **non-admin** viewer's `PlaybackInfo` `Path` carried the password
  whenever the source was not shared (`output=m3u8`);
- the server log carried it 9 times in a 6-channel refresh: once per
  channel (`xmltv path:`), and at every stream open;
- it was written to `/cache/<id>_channels` and `/cache/mediainfo/<id>.json`.

Stock Jellyfin 12.0.0 against the FAST channels (`tools/fakehdhr/fast-smoke.sh`):

- **Ingest.** For `800.2`, stock's ffmpeg opens the remote
  `master.m3u8` directly, with `-analyzeduration 3000000 -probesize 1G
  -fflags +igndts`. The media source has `SupportsDirectStream: false`,
  so the video is transcoded to H.264 and the AAC audio is **copied**.
- **SSAI ad break: the live output froze.** The client loop ran until
  +185 s. The output playlist stopped at segment 17 (+69 s, at the first
  break) and did not advance for the remaining ~2 minutes, although
  ffmpeg kept reading the input. Its log shows the video filter graph
  reconfiguring to 1920x1080 (the ad) and back to 1280x720, and **5 440
  "Non-monotonic DTS" warnings on the copied audio stream**: the break's
  new timestamps were clamped instead of handled as a discontinuity. This
  is the Apple TV freeze users report, reproduced offline. It is the
  reason for the "normalize, or carry the discontinuity" rule in §0.1 and
  for the L9 limits.
- **Expired token: a dead stream, again.** A tune of `800.4` after its
  120 s token expired returned `PlaybackInfo` with 1 media source and no
  `ErrorCode`. The log shows `Error probing live tv stream` and then
  `Live stream opened`. fakehdhr counted 1 token rejection and **no M3U
  re-fetch**: stock reads the M3U only when the guide refreshes.

Deadfin phase 1 against stock (P7, 2026-10-17, same laptop, image built
from `livetv/acceptance`). `guide-limits.sh` with `TARGET=` runs both
servers on the same fakehdhr guide. A probe toggles a favourite on the
Live TV view twice a second during the refresh and records the slowest
user-data write. Peak RSS is the server's `VmHWM`; `docker stats` memory
also counts the page cache. Deadfin's idle RSS is 185 MB.

| Run | Server | Import | Peak memory | DB | Channels / programmes | 100 ch + current | Grid 100 ch × 6 h | Slowest user write |
|---|---|---|---|---|---|---|---|---|
| L1 1 000 ch × 7 d, `-quirks all` | stock | 828 s | 824 MiB (stats) | 792 MB | 1 026 / 185 079 | 23 ms | 68 ms | not probed |
| L1 | Deadfin | **195 s** | 362 MB RSS (327 MiB stats) | **553 MB** | 1 026 / 183 461 | 19 ms | 41 ms | 470 ms |
| L2 5 000 ch × 14 d, `-quirks all` (981 MB XMLTV) | stock, `TIMEBOX=900` | cancelled at 21.3 %: 382 of 5 106 per-channel parses, about 2.4 s each, so about 3.4 h in all | 2.4 GiB (stats) | 598 MB at the stop | 5 106 / 137 370 | 39 ms | 71 ms | 616 ms (DB at 600 MB) |
| L2 | Deadfin | **2 469 s, completed** | 412 MB RSS | 5.6 GB | 5 106 / 1 845 883 | 205 ms | **56 ms** | **1.3 s steady, 6.9 s once** |
| L13 Xtream, 30 006 entries, VOD option off | Deadfin | **310 s, completed** | 527 MB RSS (+342 MB) | 590 MB | 30 006 / 182 220 | 213 ms | 40 ms | 2.3 s |
| L13, `SKIP_VOD=1` | Deadfin | **236 s, completed** | 398 MB RSS (+213 MB) | 559 MB | **10 006** / 182 804 | 78 ms | 45 ms | 848 ms |

Stock's L13 line is above: about 6 hours, cancelled at 17.4 %.

What changed to get there, found by these runs:

- **One pass over the XMLTV, programmes spilled to disk.** The first
  Deadfin L1 run peaked at 1.14 GiB, and L2 at 3.9 GB RSS: the refresh
  held every programme between the guide pass and the writes. The pass
  now encodes each programme to the temporary file of its channel batch
  (3 500 channel-days per batch, under the cache directory). A first read
  of the files collects the channel classes; a second read writes one
  batch at a time. L2 RSS is now 412 MB.
- **An index for the channel grid.** Without planner statistics, SQLite
  answered the L2 grid in 323 ms and the channels' current programmes in
  852 ms, through `ix_items_kind_top`. Migration 00020 adds
  `ix_items_top_kind_channel_start`, which the planner prefers without
  statistics (0.8 ms on a copy of the L2 database). A planner statistics
  refresh after the guide refresh also fixes the plan, but its first full
  `ANALYZE` holds the writer for 23 s on that database.
- **Channels in chunks of 500.** One transaction for all channels held
  the writer for 8.9 s at 30 006 entries.

Open against the draft 1 acceptance:

- **A user-data write waits up to about 1.3 s at L2 scale** (269 of
  3 512 probe writes over 1 s). Each channel's programme transaction
  takes that long once the items table holds about 1.8 M rows. A CPU
  profile shows the time spread over the store's per-item write path
  (item row and its 13 indexes, value maps, user-data keys, derived
  repair), growing with the table; a 256 MiB page cache does not change
  it. At L1 scale the slowest write is 470 ms.
- **Channel chunks grow with the stored channel count.** Each
  `MergeChildren` chunk reads every stored child of the folder, so at
  30 006 channels a chunk takes up to 2.3 s. With the VOD option on
  (10 006 channels) the slowest write is 848 ms.
- **L13 RSS growth is 342 MB with the VOD option off** (budget 256 MB);
  213 MB with it on.
- `channels_100_current` at L2 is 205 ms: the current-programme query
  reads the newest programme start per channel with the index, then
  sorts.
- 20 k × 14 days was not run (a 22 GB database at the L2 rate). The
  spill bounds memory by the batch size, not by the guide size.

Other P7 checks:

- `stock-smoke.sh` with `TARGET=deadfin:… TUNER=m3u PLAYBACK=0` and
  `TUNER=lineup`: a tuner from `/lineup.m3u` imports the same 23 channels,
  numbers and current programmes as one from `/playlist.m3u`.
- `provider-dryrun.sh`: the manual borrowed-account pass, done against the
  synthetic Xtream panel (500 channels, 3 000 VOD entries, `SkipVodEntries`
  on), makes **2 provider requests**: one `get.php` when the tuner is
  saved, one `xmltv.php` in the refresh the listings save queues. Reading
  the channels and the grid makes none, and a second refresh makes none
  (the playlist is cached for 15 minutes, the XMLTV for an hour). Both
  requests carry the Deadfin User-Agent; `AuthRejects`, `BanRejects` and
  `JSONEPGCalls` are 0.

### 5.4 The driver (to build)

The harness is the source side. A client-side driver plays the clients. It
goes in `tools/perfcompare` as a `livetv` subcommand, so that it reuses the
discovery, auth and report code there. For each scenario it:

1. Opens N viewers with jellyfin-web's and Swiftfin's PlaybackInfo bodies
   (`AutoOpenLiveStream: true`). It follows `TranscodingUrl` to the master
   playlist and then `live.m3u8`, and pulls segments at playback rate as a
   player does.
2. Records:
   - the time to the first segment;
   - each gap between segment arrivals;
   - errors, and the HTTP status of each.
3. Creates M timers for programmes that are airing (L7c). It checks each
   recording with ffprobe after its stop.
4. Closes everything, then polls fakehdhr `/stats.json` and the server's
   counters until they are idle or a deadline passes.
5. Writes one JSON line per run, against stock and against Deadfin, into
   the same JSONL history that perfcompare already renders.

### 5.5 Toward differential tests

1. Add a `livetv` corpus under `corpora/`. That path is protected and needs
   operator approval. Each scenario adds the fakehdhr tuner and an XMLTV
   provider with a fixed `-guide-anchor`, then replays the §2.3 client
   calls.
2. Add normalizers under `tools/difftool/normalize/` (also protected). Each
   one hides only volatile values and carries a reason: `LiveStreamId`,
   `LiveStreamFiles/<id>` path segments, `PlaySessionId`, and program `Id`s.
   Upstream derives the program ids from hashes; Deadfin must match the hash
   or carry a divergence ID.
3. Run both servers against **one** fakehdhr instance, so the tuner state is
   shared and limits behave the same. The busy-tuner case is an intentional
   difference: give it a divergence ID and keep it out of equality checks.
4. Pin the live encode arguments with golden cases taken from stock's
   `FFmpeg.Transcode-*.log` (§2.2), in the same way as the existing encoding
   golden corpus (CONVENTIONS 7.2).

### 5.6 Real sources: a sanity check, with legality

Real sources do not drive the plan. They answer one question per phase: does
the synthetic model miss something real? One short manual pass per phase
against the sources below is enough. When a real source shows a new failure,
the fix is to add that failure to fakehdhr as a quirk or fault, so CI covers
it from then on.

**Deadfin testing uses only legal, free sources:**

- synthetic tuners (`tools/fakehdhr`, local ffmpeg);
- our own fixture media, through ErsatzTV or Tunarr;
- real over-the-air reception through an HDHomeRun;
- iptv-org public streams;
- vendor test streams such as Apple bipbop;
- FAST playlists.

No CI job, test, script or session uses a real paid panel or "restream
line", and the repository names none. The Xtream path (M3U and phase 1b) is
tested against fakehdhr's synthetic panel, or a self-hosted open-source
endpoint (Dispatcharr can emit one). The operator's manual check with a
lent account follows the handling rule in §4.3: the operator alone, by
hand, on their own server.

Operators may test against any M3U or XMLTV source locally. Such sources
stay on the operator's machine through the hook in §5.7. No URL, fixture,
recommendation or CI reference to them enters the repository.

The **Use** column says where each source may appear:

- **CI / fixtures:** sources we generate, or our own media.
- **Manual:** developer machines only. Never in CI, and never as a URL in
  `corpora/`.

| Source | What it is | License / legal position | Use | Good for |
|---|---|---|---|---|
| `tools/fakehdhr` (this PR) | Synthetic HDHomeRun + M3U + XMLTV from ffmpeg lavfi patterns | Ours | **CI / fixtures** | Everything deterministic: guide matching, tuner limits, codecs, recording naming, signal drop (`-drop-after`) |
| Local ffmpeg HLS test pattern (`ffmpeg -re -f lavfi -i testsrc2 … -f hls`) | Self-made HLS "channel" | Ours | **CI / fixtures** | Separating "the stream works" from "the guide works" from "the network failed" |
| ErsatzTV / Tunarr on fixture media | HDHomeRun + XMLTV channels scheduled from our own media | Apps are open source; the content is our fixture media | **CI / fixtures** (if a container is acceptable in CI) or manual | Long-running realistic channels with real program boundaries |
| OTA through an HDHomeRun (§5.9) | Real broadcast reception | Free-to-air reception for private use | **Manual** | Real MPEG-2/AC-3 and ATSC 3.0 behavior, signal loss, the real tuner API |
| iptv-org/iptv (`https://iptv-org.github.io/iptv/countries/us.m3u`, …) + iptv-org/epg | User-submitted links to publicly available streams; an EPG grabber toolkit | The list is CC0 (GitHub reports Unlicense). Its README Legal section says the links point to streams "intentionally made publicly by the copyright holders" | **Manual** | Scale (~10 k entries), parser edge cases, and **churn**: many streams are flaky, which exercises retry, failover and dead-channel handling |
| Free-TV/IPTV (`https://raw.githubusercontent.com/Free-TV/IPTV/master/playlist.m3u8`) | A curated free-to-air list | Free-to-air only by policy | **Manual** | A smaller and steadier real-world M3U |
| FAST and station streams (the §0.1 services) | Free ad-supported channels, including local news; several also appear in iptv-org | Free to watch in their own apps. Their terms usually forbid third-party playback (§0.1), so the repository holds no URLs, generators or resolvers for them | **Manual**, operator's choice, through §5.7 only | A check that fakehdhr's `800.x` model matches real SSAI, token and geo behavior |
| Apple bipbop, Mux, Akamai test stream, Unified Streaming SCTE-35 demo (URLs in [R], test-streams section) | Vendor test streams | Published as test streams | **Manual** (external network) | Stable HLS input; SCTE-35 ad markers for commercial work |
| Dispatcharr (`Dispatcharr/Dispatcharr`) | M3U/EPG manager and stream proxy. Outputs M3U, XMLTV, Xtream Codes or an emulated HDHomeRun; automatic failover; plugins | AGPL-3.0 (run it as a separate container; never vendor or link its code) | **Manual**, and a reference design (§5.8) | A clean "upstream provider" in front of other sources, so Deadfin's integration is tested independently of where the channels come from |
| Lineuparr plugin (`PiratesIRC/Dispatcharr-Lineuparr-Plugin`) | Dispatcharr plugin that rebuilds a real provider lineup (DirecTV, DISH, FiOS, …) from your own streams, with fuzzy matching, EPG and logos | MIT | Reference only | A design reference for channel curation (5c). Do not copy its lineup files into the repository |
| Threadfin, hdhriptv | M3U → HDHomeRun proxies (hdhriptv is Go, with a shared upstream and failover) | Open source | **Manual** | A second HDHR emulator, to show that our tuner client does not fit only fakehdhr |

Rules:

- **No third-party stream or playlist URLs in `corpora/`, fixtures or CI
  configuration.** Differential scenarios use fakehdhr and local sources
  only.
- Do not commit captured broadcast, IPTV or FAST content. Keep captures for
  `-loop` outside the tree, as with the existing fixture media.
- Never put provider credentials (Xtream, Schedules Direct) in URLs, logs
  or the repository. Tests use cassettes.

### 5.7 Local private sources (never committed)

Optional real-world churn testing happens on developer machines, against whatever
sources the operator chooses: dead streams, failover, header quirks and
10 k-channel lineups. The hook keeps those sources out of the repository:

- **Location.** The source list lives in `.livetv-dev/sources.json` at the
  repository root, which this PR adds to `.gitignore`. Any other path works
  through `DEADFIN_LIVETV_DEV_SOURCES=/path/to/sources.json`.
- **Format.**

  ```json
  {"tuners":   [{"type": "m3u", "url": "…", "userAgent": "…", "tunerCount": 2},
                {"type": "hdhomerun", "url": "http://192.168.1.50"}],
   "listings": [{"type": "xmltv", "path": "… or /local/guide.xml"}]}
  ```

  These map one-to-one onto `TunerHostInfo` and `ListingsProviderInfo`.
- **Loader.** `tools/fakehdhr/dev-sources.sh <server> <token>` posts each
  entry to `/LiveTv/TunerHosts` and `/LiveTv/ListingProviders`. It works
  against stock Jellyfin and against Deadfin, because both take the same
  API. It refuses a sources file that sits inside the repository without
  being gitignored, and it prints only types and ids, never URLs.
- **The server stays clean.** Deadfin gets no development-only code path.
  Private sources reach it through the normal Live TV configuration in the
  data directory, which is never in the repository.
- **Logs.** The phase 1 tuner code must redact credentials in M3U and
  Xtream URLs, the user-info and query parts, before logging. That protects
  both private test sources and production users.

### 5.8 Dispatcharr as a reference design

Dispatcharr already does what gaps 2, 4 and 9 ask for, in front of any media
server. It is worth studying before 5c and 5g:

- **Failover.** A channel holds an ordered list of streams. On a failure,
  the proxy switches to the next stream without dropping viewers. This is
  the broker behavior proposed in 5g. Our version needs to keep the segment
  ring (5a) running across the switch.
- **Curation.** Groups, filters, renumbering and EPG auto-matching, and with
  Lineuparr, "make my channels look like a DirecTV lineup". This is the
  upper end of 5c.
- **Positioning.** Many power users will keep running Dispatcharr *and* a
  media server. Deadfin should therefore (a) work well behind Dispatcharr's
  M3U, XMLTV and HDHR outputs (a manual compatibility check in each phase)
  and (b) add native curation only where stock clients benefit: hidden
  channels, ordering and groups as tags. A full Dispatcharr clone is not
  the goal.

### 5.9 What to buy or borrow

Nothing is needed for phases 1 and 2: the anchor use case runs on internet
HLS, and fakehdhr covers the tests. Hardware matters only for phase 3, and
then **at the remote site with good signal**, not at the household. Check
that site's reception first: enter its address in the FCC DTV Reception
Maps (https://www.fcc.gov/media/engineering/dtvmaps) or RabbitEars'
signal search. The FCC predictions assume an outdoor antenna 30 ft above
ground, so expect less indoors.

| Item | Price (2026-10-12) | Why |
|---|---|---|
| **HDHomeRun FLEX 4K** | $199.99 ($174.99 refurbished) | One box covers ATSC 1.0 MPEG-2 1080i/AC-3 and ATSC 3.0 (HEVC, AC-4) on 2 of its 4 tuners. DRM channels fall back to 1.0. It also exercises the 4-tuner limit and HTTP streaming |
| HDHomeRun FLEX QUATRO | $149.99 | 4 tuners, ATSC 1.0 only. The cheaper choice where no ATSC 3.0 station is wanted, or where the 3.0 stations are DRM or AC-4 only (§0.2) |
| An indoor or attic antenna at the remote site | ~$30–60 | Height and line of sight matter most. VHF-high stations need a VHF-capable antenna. Add an LTE/5G filter (SiliconDust LPF-608M, $19.99) near cell towers |
| Optional: HDHomeRun FLEX DUO | $109.99 | A second host, for pooled tuner counts and failover between two real devices |
| Cheaper alternative: a used HDHomeRun CONNECT or FLEX | used market; price not checked | Any model with HTTP streaming (DLNA 2015+, so Emby works too) serves the same `discover.json`/`lineup.json`/`/auto/v` contract. Check that the model streams over HTTP, not legacy UDP only |
| Optional: Schedules Direct | $35/yr ($9 for 2 months; 7-day trial) | Needed only for the SD client (gap 1). **Do not point stock Jellyfin 12.0 at a real SD account during differential runs.** #18045 shows the account can be blocked. Use cassettes |
| Borrow | the community member's setup | Ask for their M3U and XMLTV shapes (with credentials removed), their Emby DVR settings, and a list of the Emby features they use. That turns the ranking in §3 into a target list |

Not worth buying: HDHomeRun legacy models (rare, UDP-only path), Hauppauge
cards (Windows-only in Emby), SCRIBE 4K (a FLEX 4K with a disk).

---

## 6. Phased plan

### 6.0 Decision D20

D2 deferred Live TV "entirely". The operator reversed it on 2026-10-13
(§6.3), as D20 in `docs/00-overview.md`:

> **D20** — Reverses D2 for Live TV and SyncPlay. Live TV ships at upstream
> parity first, off by default, with no tuners configured. Each extra
> after parity has its own divergence row.

Live TV is a milestone after v0.3.0. Divergence D-07 shrinks with each
phase and is retired at the end of phase 4. SyncPlay gets its own spike.

### 6.1 Phases

| Phase | Scope | Effort | Client work |
|---|---|---|---|
| **1. M3U + XMLTV** | Live TV config; M3U tuner with HTTP(S) TS and HLS URLs; M3U refresh separate from the guide; XMLTV; channels and programs as items; Refresh Guide task; `livetv` user view; the read routes | L | none |
| 1b. Xtream tuner type | On the phase 1 `Host` seam: `player_api` categories and live streams, a category picker, the guide from bulk `xmltv.php` only; a divergence row (§4.3) | S–M | deadfin-web (setup) |
| **2. Live playback** | Stream broker that shares TS **and HLS** sources; remux or normalize for AVPlayer; discontinuity-correct live HLS; re-fetch on expired URLs; clear geo and busy errors; WAN bitrate cap | L | none |
| **5g. Multi-source channels, failover and IPTV** | Logical channels with staged matching, overrides and one canonical guide (§4.4); ordered failover in the broker (reference: Dispatcharr, §5.8); headers; `tvg-shift`; catch-up. | L | deadfin-web (setup) |
| **4. DVR at parity** | Timers and series timers; recorder; recording libraries; NFO; post-processor; WS timer events; recording routes | L | none |
| 5d. SharkParty Live TV (in parallel with phase 2) | EPG grid out of Experimental, mini-guide, channel up/down, tvOS checks | M | SharkParty, against stock Jellyfin plus fakehdhr |
| 5a. Time-shift | Segment ring per channel; rewind; watch from start | L | verify per client |
| 5b. Rules and conflicts | Apply ignored fields; conflict prediction; `ConflictedOk/NotOk`; keyword rules | M | deadfin-web (keyword rules) |
| 5c. Channel management | Groups → tags; hide, renumber, order; logo source (reference: Dispatcharr and Lineuparr, §5.8) | M | deadfin-web |
| 5e. Guide sources | Merge multiple XMLTV sources; Schedules Direct client that obeys its limits (cassettes) | M + M | none |
| 5f. Commercial skip | MediaSegments routes (deferred today), then comskip → EDL → `Commercial` segments | M + M | check Swiftfin |
| 3. Remote network tuner (**parked**) | HDHomeRun tuner by URL over a VPN (HTTP API, the "hdtc" check, UDP discovery on the LAN); WAN tolerance; interlace probe and the QSV, VAAPI and software deinterlace branches; AC-4 and DRM marking | M | none. Parked: a local HDHomeRun works in phase 1 through its `/lineup.m3u`; remote tuning comes back later |

Testing split for every phase:

- **CI** relies on fakehdhr and local ffmpeg sources only. Unit and
  fixture tests are deterministic and run offline.
- **Limits** come from the §5.2 scenarios, run on demand with fakehdhr.
  Each phase owns its rows: 1 owns L1–L3, L13 and L14 (import side); 2
  owns L4, L5, L7a–b, L9–L11, L14 (playback side), L15 and L16; 3 owns L12; 4 owns L7c; 5g owns L6 and L17. Every phase reruns L8 before
  release. A phase's
  acceptance records the run's JSON line next to stock's.
- **Real sources** get one short manual sanity pass per phase (§5.6). A
  new failure found there becomes a fakehdhr quirk or fault. Sources are
  named only by type, never by URL.

M3U plus XMLTV comes first because the anchor use case (§0) is live
internet HLS. It is also the lowest common denominator: every IPTV source,
Dispatcharr, Threadfin, ErsatzTV and fakehdhr (`/playlist.m3u`) can feed
it, and the HDHomeRun's own `/lineup.m3u` can too. The HDHomeRun API comes
later, on the same channel and program model, when phase 3 comes back.

The order is the operator's (2026-10-13): 1 → 1b → 2 → 5g → 4 → the rest
(5a → 5b → 5c → 5e → 5f). 5g comes before the DVR because the operator
runs three IPTV providers whose channel lists overlap. 5d runs in parallel
with phase 2, in the SharkParty repository. Phase 3 is parked.

### 6.2 Issue specs

Every issue below follows AGENTS.md: one issue, one branch, one pull request
into `master`; Gate A green; the operator merges. The filed issues are in
the "Live TV" milestone: phase 1 #541, 1b #542 (from §4.3), 2 #543, 5g
#544, and 5d #545 (tracking; the work is in the SharkParty repository).
Phase 4 is filed when 5g is under way. Draft 3 is parked with its phase.
Draft 0 can start at any time, because it changes no server behavior.
SyncPlay has its own milestone, starting with #546.

---

#### Issue draft 0 — Live TV limits harness: driver and counters

This issue changes no server behavior.

**Goal.** Run any §5.2 scenario with one command against stock and against
Deadfin. The run writes one comparable JSON line.

**Mechanism.**

- Commit `tools/fakehdhr` from this spike, with its tests.
- Add `perfcompare livetv -scenario L1|…|L8 -target <url>`, as described
  in §5.4.
- Add `guide-limits.sh` as the L1 shell form.
- Add a read-only Live TV counters snapshot to the pprof listener
  (§5.2). It reports zeros until phase 1 fills it.

**Acceptance.**

- `go test -race ./tools/fakehdhr ./tools/perfcompare/...` passes, with
  no ffmpeg and no sleeps.
- L1 at 1 k × 7 days and L4 run against stock 12.0.0. Their JSON lines are
  attached to the pull request as the first reference values.
- `perfcompare` renders the lines into the existing history view.

**File surface.** `tools/fakehdhr/**`, `tools/perfcompare/**`,
`cmd/deadfin/pprof.go`.

**Exclusions.** Gate A runs no scenario, because they take minutes and need
Docker and ffmpeg. No external sources. No change to `corpora/`.

---

#### Issue draft 1 — Live TV phase 1: M3U tuner, XMLTV guide, channels and programs

**Goal.** An administrator can add an M3U tuner and an XMLTV listing
provider in stock jellyfin-web. After Refresh Guide runs, the Live
TV view in jellyfin-web and Swiftfin/SharkParty lists the channels with
logos, current programs and a guide grid, matching stock Jellyfin 12.0.0
fed from the same `tools/fakehdhr` instance (`/playlist.m3u` and
`/xmltv.xml`), including the `800.x` HLS channels. A local HDHomeRun works through its
`/lineup.m3u` as an M3U tuner. Nothing plays yet. The HDHomeRun tuner type
is parked with phase 3; the tuner seam keeps room for it.

**Mechanism.**

- *Configuration.* Register the named configuration `livetv` in
  `internal/config` with the `LiveTvOptions` field list
  (`docs/spikes/livetv-upstream.md` §5). `TunerHostInfo` and
  `ListingsProviderInfo` keep the upstream wire shapes. Ids are 32-hex.
- *Tuners.* New package `internal/livetv/tuner`, with a tuner-host
  interface shaped so that the phase 3 `HDHomeRun` type drops in:
  - `M3U`: port `M3uParser`, keeping its attribute precedence (`tvg-id`,
    `channel-id`, `tvg-chno`, `tvg-logo`, `group-title`) and its accepted URL
    schemes. Port its upstream tests. Entries may point at TS or HLS
    (`.m3u8`) URLs.
  - The M3U source is re-read on its own schedule (a per-tuner refresh
    interval, default with the guide) and on demand by the phase 2 broker,
    without a full guide refresh. Every logged or wrapped source URL goes
    through the §4.3 redaction, which masks query secrets, user info and
    secret values in channel paths.
  - A per-tuner option skips VOD entries (§4.3), defaulting to parity.
  - Channel ids follow upstream (the M3U hash form now, and
    `hdhr_<GuideNumber>` in phase 3), so that mappings and user data keys
    line up with imported databases.
  - The tuner-host interface is the `Host` seam of §4.3; the phase 1b
    Xtream type is its third implementation.
  - Every provider request carries `Deadfin/<version>`, or the tuner's
    `UserAgent` override (§4.3).
  - Tuners are context-first, take an injected `*http.Client`, and hold no
    package state.
- *Guide.* New package `internal/livetv/guide`:
  - A streaming XMLTV reader that parses once per refresh.
  - Channel matching in upstream order (id → tuner channel id → number →
    name) through `ChannelMappings`.
  - A guide window of `GuideDays` (default 7).
  - The Refresh Guide task (`RefreshGuide`, category "Live TV", every 24 h)
    on `internal/tasks`. It is queued again after any tuner, provider or
    mapping change.
- *Store.* Channels become `KindLiveTvChannel` items and programs become
  `KindLiveTvProgram` items. Per-program fields (start, end, channel,
  episode numbers, `IsNew`/`IsPremiere`/`IsLive`, the category flags,
  `ShowId`) go in a new satellite table, with an index on
  (channel, start) and on (start, end). SQLite and PostgreSQL migrations
  land together. All SQL stays in `internal/store`. The storetest
  conformance suite covers it.
- *API.* Replace these stubs in `internal/api/handlers/livetv.go`, keeping
  their policies:
  - `Info`, `TunerHosts` (POST and DELETE), `TunerHosts/Types`;
    `Tuners/Discover` and `Discvover` keep returning `[]` until phase 3;
  - `ListingProviders` (POST and DELETE), `ListingProviders/Default`,
    `ListingProviders/Lineups` (XMLTV form only);
  - `ChannelMappingOptions`, `ChannelMappings`;
  - `Channels`, `Channels/{id}`;
  - `Programs` (GET and POST), `Programs/{id}`, `Programs/Recommended`;
  - `GuideInfo`.
  
  Add the `livetv` view to `/UserViews` when at least one tuner exists and
  the user has `EnableLiveTvAccess`. Update `ledger/annotations.yaml` and
  regenerate the ledger.

**Acceptance.**

- `go test ./internal/livetv/... ./internal/store/... ./internal/api/...`
  passes, on both store backends.
- The ported `M3uParser` tests pass, and each cites its C# source file.
- A fixture run (an extension of `tools/fakehdhr/stock-smoke.sh` pointed at
  Deadfin) yields:
  - the same channels (the 6 base and the 8 `800.x` HLS channels), with
    the same `ChannelNumber`, `Name` and `Type` (`TvChannel`);
  - a `Primary` image tag on each channel;
  - `CurrentProgram.Name` equal to stock's at a fixed `-guide-anchor`;
  - a `livetv` user view.
- A tuner added from fakehdhr's `/lineup.m3u` (the HDHomeRun name for the
  playlist) imports the same channels as `/playlist.m3u`.
- `POST /LiveTv/Programs` for 6 channels × 3 days returns the same program
  names and start dates as stock.
- jellyfin-web: the Live TV Guide and Channels tabs render with no console
  errors. SharkParty on tvOS: the Live TV tab lists the channels.
- Limits (§5.2 L1–L3), with `guide-limits.sh` against Deadfin and stock at
  the same anchor:
  - 1 k channels × 7 days and 5 k × 14 days, `-quirks all`: the refresh
    completes. Import time and peak memory are no worse than stock's.
    20 k × 14 days completes in bounded memory, even where stock does not.
  - The 100-channel × 6 h grid query has p95 no worse than stock and
    ≤ 250 ms (provisional; draft 0 sets it) at 5 k × 14 days on
    SQLite, and the query plan uses an index.
  - A playstate write during the refresh is never blocked for more than
    1 s; the refresh commits in batches.
  - Every quirk imports: duplicates collapse to one channel, both DST hours
    keep their programmes, and channels without a `tvg-id` match as stock
    matches them.
- Limits (§5.2 L13), Xtream scale (30 k `m3u_plus` entries, 98 MB XMLTV):
  - Go benchmarks of the M3U and XMLTV parsers with `-benchmem` show
    allocation flat in the input size: the parsers stream, and only the
    current entry or programme is held.
  - The whole import (M3U, channel sync, guide) completes. Peak RSS growth
    is below 256 MB (provisional), and the result is recorded next to
    stock's L13 line.
  - With the VOD option on, the 20 k `/movie/` and `/series/` entries are
    skipped; with it off, the result matches stock.
- User-Agent (§4.3): an `httptest` provider sees `Deadfin/<version>` on the
  M3U and XMLTV fetches, and the override when one is set. Nothing is
  rejected by fakehdhr `-block-default-ua`.
- Provider politeness (§4.3, L16 a and d): a wrong password reaches
  "credentials rejected" after one M3U request, with no further requests.
  A 403 storm backs off exponentially. `BanRejects` stays 0.
- Credentials (§4.3, L14 import side): Go tests with a synthetic marker
  password in a `get.php`-style tuner URL, an `xmltv.php`-style listings
  URL and `/live/U/P/` channel URLs. Add the tuner, refresh, and fail one
  fetch. The marker appears in no captured log line, no error string, no
  non-admin API response, and no file outside the stored configuration.
- Run `go vet ./...` and `go run ./tools/convention-gates -root .`; both
  pass.

**File surface.**

- `internal/livetv/{tuner,guide}/` (new)
- `internal/config` (the `livetv` named configuration)
- `internal/store`: migrations for both dialects, and the program satellite
- `internal/api/handlers/livetv.go`, plus the user-views handler
- `internal/tasks` registration in `cmd/deadfin`
- `ledger/annotations.yaml`, `ledger/*` (regenerated)
- `docs/divergences.md`: narrow D-07
- `tools/fakehdhr/stock-smoke.sh`: a `TARGET` switch

**Exclusions.**

- The HDHomeRun tuner type and tuner discovery (phase 3).
- Playback, `LiveStreams/Open`, and the `LiveStreamFiles` route (phase 2).
- Timers and recordings (phase 4).
- Schedules Direct: `ListingProviders/SchedulesDirect/Countries` stays
  stubbed.
- Legacy UDP HDHomeRun tuners.
- Channel hide, groups and renumbering (5c).
- Merging several XMLTV sources (5e).

---

#### Issue draft 2 — Live TV phase 2: live playback through a shared stream broker

**Goal.** Stock jellyfin-web and SharkParty play live channels from
`tools/fakehdhr` through the M3U tuner:

- FAST-style HLS (`800.x`): through ad breaks, expired tokens and stale
  playlists, with no freeze on Apple TV;
- H.264 with AAC over TS (`4.1`, `5.1`): remuxes or direct-streams.

A second viewer of the same channel shares one upstream. When every tuner
is busy, the source is geo-blocked or a URL stays dead, the client gets a
playback error and not a dead stream. Closing releases the tuner.

**Mechanism.**

- *Broker.* New package `internal/livetv/stream`, which implements
  `playback.LiveStreamProvider` and is composed in `cmd/deadfin`.
  - One supervised reader goroutine per open (host, channel, profile). For
    a TS source it writes the TS to a buffer file under the transcode path,
    as upstream `SharedHttpStream` does. For an HLS source it is one ffmpeg
    ingest (or a playlist follower) per channel, which upstream never
    shares (`M3UTunerHost.cs:97-129`). Consumers are counted by reference.
  - On a 401, 403, 404 or 410 at open or mid-stream, re-read the M3U
    source once (single-flight, rate-limited), take the new URL for the
    same channel id, and retry. A geo 403 that survives the re-read
    returns the `NotAllowed` playback error.
  - Opening is not done under a global lock. Upstream holds one lock across
    the whole tuner connect [U§9.5].
  - `TunerCount` is enforced per host before connecting. When a host is
    full, the broker tries the next host that has the same channel id.
  - Close runs from `LiveStreams/Close`, playstate stop, session end and
    transcode kill.
  - The idle timeout runs on `clock.Clock`. `Close(ctx)` is goleak-tested.
- *Busy tuner.* When no tuner is free, `PlaybackInfo` with
  `AutoOpenLiveStream` returns the existing `ErrorCode` response and
  `LiveStreams/Open` returns its error status. Upstream returns 200 with a
  dead source (§2.2). Add a divergence row.
- *Media source.* Return the upstream live shape: `Protocol: Http`,
  `IsInfiniteStream`, `RequiresOpening`, `RequiresClosing`, `LiveStreamId`,
  `AnalyzeDurationMs: 3000`, `IgnoreDts`, and `Path` set to
  `/LiveTv/LiveStreamFiles/{id}/stream.ts`. Probe after the open, with the
  3-second wait. Do not cache the probe forever [U§9.6].
- *Encode.* In `internal/encoding`:
  - accept a non-file input (the broker URL, or a broker-owned file reader
    path);
  - add the pure live input modifiers: `-analyzeduration`, `-probesize`,
    `-fflags +igndts`, `+genpts` on video copy, `-re` when
    `ReadAtNativeFramerate` is set, and the reconnect flags when the source
    loops;
  - choose **remux** while an HLS source keeps one shape, and **normalize**
    (transcode to one fixed resolution, frame rate and audio layout) when
    the source carries SSAI discontinuities or changes shape (§0.1);
  - cap the live bitrate by `RemoteClientBitrateLimit` and the client's
    `MaxStreamingBitrate`.
  
  Golden cases come from stock's ffmpeg logs (§2.2).
- *HLS.* In `internal/ffmpeg`, add an infinite-source HLS job mode:
  `-hls_playlist_type event -hls_list_size 0`, the `hls/<name>/` base URL,
  no seek-respawn, the existing inactivity kill and segment cleaner. The
  master playlist points at `live.m3u8`. The output follows Apple rules
  8.13–8.17: when Deadfin does not normalize, it carries each source
  discontinuity as `EXT-X-DISCONTINUITY` with a correct
  `EXT-X-DISCONTINUITY-SEQUENCE`. SCTE-35 cue times are kept as stream
  markers for later use (5f). Relax `validateHLSSource` in
  `internal/playback/hls_service.go` only for broker-owned sources.
  Trickplay stays off.
- *Route.* Serve `GET /LiveTv/LiveStreamFiles/{streamId}/stream.{container}`
  from the broker. It stays anonymous, as upstream does; the ids are random
  and expire with the stream.

**Acceptance.**

- `go test -race ./internal/livetv/... ./internal/ffmpeg/... ./internal/encoding/... ./internal/playback/...`
  passes, including:
  - fake-clock idle close;
  - goleak on broker `Close`;
  - a tuner-limit test (2 tuners, 3 channels → third is a `PlaybackInfo`
    error);
  - a sharing test (2 viewers, 1 upstream connection, counted by fakehdhr
    `/status.json`).
- The encoding golden cases for the live MPEG-2 and H.264 inputs match
  stock arguments, apart from the documented divergences.
- The chaos test is extended to live: zero orphan ffmpeg processes and zero
  held tuners after 50 open, play and close cycles.
- Manual: jellyfin-web and SharkParty (tvOS) play `800.2` and `4.1` for 10
  minutes (across 4 ad breaks) with no stall, and fakehdhr `/status.json` shows the tuner
  released within the idle timeout after stop.
- Limits (§5.2 L4, L5, L7a–b), with the driver against fakehdhr
  `-source cache`:
  - The first segment arrives no later than on stock, for each profile.
  - `900.8` hang and `900.9` dead give a clear error within the open
    timeout. `900.3` die ends the stream cleanly. Each one leaves 0 held
    tuners and 0 ffmpeg processes.
  - `900.2` stall and the `900.4`–`900.7` splices recover, or end with an
    error. They never leave a frozen player with a live ffmpeg.
  - 100 viewers on one channel: 1 tuner open and 1 ffmpeg per output
    profile.
  - 200 viewers on 50 channels with `-tuners 8`: exactly the expected
    rejections, each one a `PlaybackInfo` error.
- Credentials (§4.3, L14 playback side): with the marker URL, through
  open, play, a forced source error and close:
  - `PlaybackInfo` `MediaSources[].Path` is the broker URL for TS and HLS
    sources, with sharing on and off. This is a divergence row, approved
    by the operator, because upstream returns the raw URL for unshared
    sources. Use the next free D-number on `master` at merge time, and
    check again after each rebase;
  - a test with a **non-admin** user and a tuner URL that holds fake
    credentials asserts that no field of any response (`PlaybackInfo`,
    `LiveStreams/Open`, items, channels, sessions) contains them;
  - the marker appears in no log line, including the ffmpeg command line;
  - the marker appears in no file under the cache and transcode paths.
- Provider politeness (L16 b and c): a password change during playback
  ends the streams with one clear error each and one rejected-state
  transition, not a retry per viewer. 50 viewers on 10 channels with
  `TunerCount` 4 never make more than 4 upstream connections.
- User-Agent in ffmpeg (§4.3): golden cases show `-user_agent` in every
  live ffmpeg argv, including probes. The value is the override, or the
  default. ffmpeg never sends `Lavf`.
- Long-running TS (L15): one channel open for 12 h. The broker's buffer is
  bounded: a ring or a rotating file, not upstream's single growing `.ts`
  (`SharedHttpStream` opens it with `FileMode.Create` and writes until
  close, about 3.6 GB per hour at 8 Mbit/s). RSS is flat, and the output
  is still in sync at the end.
- Limits (§5.2 L9–L11):
  - `800.2`, `800.3` and `800.8` play across 10 breaks with no stall above
    one segment on AVPlayer, and with 1 upstream fetch per segment for 20
    viewers.
  - `800.4`: a tune after expiry plays within 5 s with 1 M3U re-fetch;
    20 viewers at expiry cause 1 re-fetch.
  - `800.5`: a `NotAllowed` error within 2 s and no retry storm.
  - A live channel through the public ingress with a 4 Mbit/s cap plays
    with no stall.

**File surface.**

- `internal/livetv/stream/` (new)
- `internal/playback/{playback_info.go,hls_service.go}`
- `internal/encoding/{hls.go,progressive.go}` and `testdata` golden cases
- `internal/ffmpeg/{hls_job.go,transcode_manager.go}`
- `internal/api/handlers/{livetv.go,playback_info.go,hls_stream.go}`
- `cmd/deadfin` composition
- `docs/divergences.md`: the new busy-tuner row

**Exclusions.**

- Time-shift, segment rings and rewind (5a).
- Failover in the middle of a stream (5g).
- HDHomeRun on-device transcode profiles beyond `native`.
- Legacy UDP tuners.
- Recording (phase 4).
- The HDHomeRun tuner type, MPEG-2 and interlaced input, and the
  deinterlace branches (phase 3).
- Hardware-accelerated live decode, except where the existing hwaccel path
  works unchanged.
- Any per-service URL resolver, scraper or playlist generator (§0.1).

---

#### Issue draft 3 (parked) — Live TV phase 3: remote network tuner (HDHomeRun over a VPN)

**Goal.** An administrator adds an HDHomeRun at another location by URL,
over WireGuard or Tailscale. Its ATSC 1.0 channels play on Apple TV and in
jellyfin-web as deinterlaced H.264 over HLS, with hardware acceleration
where the host has it. The tuner survives a slow, lossy link, or says
clearly that the link is too slow. ATSC 3.0 AC-4 and DRM channels are
marked, not silently broken.

**Mechanism.**

- *Tuner.* Add the `hdhomerun` type to `internal/livetv/tuner`:
  - `discover.json`, `lineup.json` and `/auto/v<n>`, with the upstream
    `hdhr_<GuideNumber>` channel ids;
  - the model "hdtc" transcode check;
  - the `X-HDHomeRun-Error` codes 804, 805 and 807 mapped to playback
    errors;
  - DRM channels hidden, as upstream does (`HdHomerunHost.cs:84`);
  - UDP discovery on 65001 for the LAN only. Over a VPN, the tuner is added
    by URL.
- *WAN tolerance.* Per-host settings (defaulted from a measured round trip
  at add time):
  - open timeout;
  - read buffer;
  - one reconnect to the same channel after a stall shorter than N
    seconds.
  
  At open, the broker measures the first seconds of throughput against
  the stream rate. Below the rate, it returns a clear playback error
  instead of a stalling stream.
- *Interlace and deinterlace.* Record `IsInterlaced` (field order) for
  live probes. This narrows D-37 for live sources. Port upstream's
  deinterlace branches:
  - software `yadif`/`bwdif`;
  - QSV `deinterlace_qsv=mode=2` and the `vpp_qsv` form
    (`EncodingHelper.cs:3674`);
  - VAAPI `deinterlace_vaapi`.
  
  The branches go into `internal/encoding/hwaccel.go`, with golden cases
  from stock logs. 30i goes to 60p, per Apple rule 1.15.
- *ATSC 3.0.* Probe the audio codec. With no AC-4 decoder in the bundled
  ffmpeg, mark the channel's audio unsupported, so the client shows a
  reason. HEVC video goes out as fMP4 HLS for Apple (rule 1.5), or is
  transcoded to H.264.

**Acceptance.**

- `go test -race ./internal/livetv/... ./internal/encoding/...` passes,
  with golden cases for software, QSV and VAAPI deinterlace.
- A fixture run against fakehdhr by URL: the `2.1` (1080i MPEG-2 + AC-3
  5.1) and `2.2` (480i) output probes as progressive H.264 + AAC on
  jellyfin-web and SharkParty.
- Limits (§5.2 L12):
  - `cap=` at 2× the stream rate with `latency=150ms` and 0.5 % netem loss
    plays for 10 minutes with no stall.
  - At 0.5× the rate, the error comes within 10 s.
  - Neither case leaks a tuner or an ffmpeg process.
- Manual (when hardware is on hand): a real HDHomeRun over a VPN plays one
  ATSC 1.0 channel on the QSV host. An ATSC 3.0 AC-4 channel shows the
  unsupported-audio reason.

**File surface.** `internal/livetv/tuner`, `internal/livetv/stream`,
`internal/encoding/{hwaccel.go,hls.go}` and `testdata`, `internal/ffmpeg`
probe, `internal/api/handlers/livetv.go` (`Tuners/Discover`),
`docs/divergences.md` (D-37 narrowed).

**Exclusions.** A transcode relay at the remote site. Legacy UDP tuners.
Any sharing of tuners or channels outside the server's own users (§0.2).
DRM playback.

---

#### Issue draft 4 — Live TV phase 4: DVR at parity (timers, series timers, recordings)

**Goal.** From stock jellyfin-web or SharkParty, a user can:

- record one program;
- create a series rule;
- see scheduled and in-progress recordings;
- play a finished recording from an auto-created recording library.

Behavior matches stock 12.0.0 for the fields upstream applies. Fields that
upstream ignores keep being ignored in this phase.

**Mechanism.**

- *Store.* Timers and series timers go in SQL tables, not upstream's
  whole-file JSON. The wire DTOs stay the same. Migrations land for both
  dialects.
- *Scheduler.* `internal/livetv/dvr` runs on `clock.Clock`. It turns series
  timers into timers with upstream's rules: `RecordAnyTime`,
  `RecordAnyChannel`, `RecordNewOnly`, `SkipEpisodesInLibrary`, `KeepUpTo`,
  pre and post padding. It reschedules after every guide refresh. A failed
  start retries every 60 s, up to 10 times, as upstream does.
- *Recorder.* A recording is a broker consumer, so it shares a tuner with a
  viewer of the same channel. It copies the TS directly. When the source is
  not TS, it remuxes through `internal/ffmpeg` with stream copy, passing the
  tuner's HTTP headers.
- *Naming and libraries.* Paths follow upstream naming, for example
  `…/Series/Season N/Series SxxEyy Title.ts`. `RecordingPath`,
  `MovieRecordingPath` and `SeriesRecordingPath` are honored. Recording
  folders are created and registered as libraries through
  `internal/library`. An NFO and images are written when `SaveRecordingNFO`
  and `SaveRecordingImages` are set.
- *Post-processor.* After a recording, run `RecordingPostProcessor` with
  `{path}` substitution through supervised process control. Its stdout and
  stderr are logged; upstream drops them.
- *Events.* `TimerCreated`, `TimerCancelled`, `SeriesTimerCreated` and
  `SeriesTimerCancelled` go out over `internal/events` → WS to users with
  Live TV access.
- *API.* Replace the stubs for:
  - `Timers` (all CRUD), `Timers/Defaults`;
  - `SeriesTimers` (all CRUD);
  - `Recordings`, `Recordings/{id}` (GET and DELETE), `Recordings/Folders`;
  - `LiveRecordings/{id}/stream`;
  - the hidden `Recordings/Series` and `Recordings/Groups`, which return
    upstream's empty obsolete shapes.
  
  Retire divergence D-07.

**Acceptance.**

- `go test -race ./internal/livetv/dvr/...` passes on a fake clock:
  - a series rule over the fakehdhr guide (`<new/>` on 2 of every 3
    airings) creates the same timers as stock at the same `-guide-anchor`;
  - padding is applied;
  - `KeepUpTo` deletes the oldest recording;
  - a cancelled timer stops the recording and releases the tuner.
- Storetest passes on both backends.
- Fixture run: recording the airing program on 4.1 produces the same file
  name and NFO fields as stock (§2.2), and the item appears in the
  recordings library after the refresh.
- Manual: jellyfin-web Schedule and Series tabs, and the SharkParty record
  button, create and cancel timers. The WS timer events reach a second
  open client.
- Limits (§5.2 L7c): 20 recordings and 50 viewers at once on `-tuners 0`.
  Every recording completes, its size is within 10 % of bitrate ×
  duration, and ffprobe shows no gap longer than 1 s. A `die=` fault
  during a recording ends it with a partial file and a recorded error.

**File surface.**

- `internal/livetv/dvr/` (new)
- `internal/store`: timer and series-timer tables, both dialects
- `internal/library`: registering recording folders
- `internal/events` and `internal/ws`: timer message types
- `internal/api/handlers/livetv.go`
- `cmd/deadfin`
- `ledger/*`
- `docs/divergences.md`: retire D-07; add a row for SQL timer storage only
  if any behavior is observable

**Exclusions.**

- Applying `Priority`, `KeepUntil`, `Days` and required padding (5b).
- Conflict prediction (5b).
- Keyword rules (5b).
- Commercial detection (5f).
- Watching an in-progress recording from its start (5a).
- Native conversion to MKV or MP4 (later; S).

---

#### Issue draft 5 — Live TV 5g: multi-source logical channels with failover

**Goal.** With merging turned on, channels that several tuners carry
appear once. Playback fails over across their sources in a set order.
Favorites, user data and recordings stay on the logical channel when
sources come and go. With merging off, the output equals parity.

**Mechanism.** As §4.4 describes:

- the normalizer and the five matching stages, each recording its stage
  and confidence;
- override storage (merge, split, pin, exclude) keyed by the stable source
  key, never the URL;
- the store tables `livetv_logical_channel`, `livetv_channel_source` and
  `livetv_merge_override`, with SQLite and PostgreSQL migrations together;
- choosing a sticky canonical guide;
- ordered failover in the broker's open path, with per-provider connection
  limits and §4.3 backoff;
- the native API for override management and suggestions (deadfin-web).

**Acceptance.**

- Normalizer table tests cover prefixes, quality suffixes, punctuation and
  timeshift markers. `X +1` never matches `X`.
- L17 with the three personas: pairwise precision ≥ 0.99 and recall
  ≥ 0.97 against `/truth.json`. Fuzzy matches appear only as suggestions.
- Failover: with `a`'s source returning 503, the open succeeds on `b`
  within one retry. With `b` at its tuner limit, the open goes to `c`. A
  401 takes its tuner out and does not retry.
- Overrides and logical ids are unchanged after 3 refreshes and after a
  provider renames 20 channels. A favorite and a series timer on a merged
  channel survive the removal of its preferred source.
- Programme lists of a logical channel come from exactly one guide.
- With the option off, the stock comparison gives the same channel list as
  stock.
- A divergence row is added in the same change.

**Exclusions.** Mid-stream failover (broker work after phase 2), catch-up,
and merging across channel numbers that the user set manually.

### 6.3 Decisions (operator, 2026-10-13)

Recorded on #484.

1. **D2 is reversed for Live TV and SyncPlay**, as D20 (§6.0). Live TV
   ships at upstream parity first, off by default. Each extra comes after,
   with its own divergence row.
2. **Live TV is a milestone after v0.3.0.**
3. **Phase order:** 1 → 1b (Xtream) → 2 (playback) → 5g (multi-source
   merging and failover) → 4 (DVR) → the rest of 5x.
4. **Phase 3 (remote HDHomeRun) is parked.** A local HDHomeRun works in
   phase 1 through its `/lineup.m3u`.
5. **SharkParty 5d starts in parallel with phase 2**, against stock
   Jellyfin plus fakehdhr.
6. **`corpora/` and the normalizers** are approved in principle after
   phase 1. Each corpus or normalizer change is still its own protected
   PR for the operator's approval.
7. **FAST position confirmed** (§0.1): Deadfin plays any well-formed live
   HLS; users bring their own sources (M3U, Xtream, HDHomeRun).
8. **No community setup survey.** The order above stands.
9. **The `PlaybackInfo` `Path` divergence is approved** (§4.3): every field
   that would carry a raw tuner or provider URL is the broker URL, for
   every user.
