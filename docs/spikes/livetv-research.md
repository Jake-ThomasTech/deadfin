# Live TV / DVR gap analysis: Emby vs Jellyfin (for Deadfin)

Researched 2026-10-12. Current versions at research time: Jellyfin server 12.1/12.2
(the 12.0 release dropped the "10." prefix; what would have been 10.12 is 12.0,
released 2026-09-07 — https://jellyfin.org/posts/jellyfin-release-12.0 ,
https://en.wikipedia.org/wiki/Jellyfin). Emby Server 4.9.x stable, 4.10 beta
(https://emby.media/community/topic/21348-emby-latest-versions).

Method note: Reddit is not reachable by the research crawler (domain blocked),
so Reddit volume could not be measured directly. Demand is ranked with
features.jellyfin.org (Fider) vote counts (pulled through the Fider API on
2026-10-12), GitHub reaction counts on `jellyfin/jellyfin` issues with label
`live-tv`, and repeated complaints on forum.jellyfin.org and emby.media/community.

Raw demand signal (Fider votes, Live-TV-related, top entries):

| Votes | Status | Request | Link |
|---|---|---|---|
| 377 | planned | IPTV Channel Groupings | https://features.jellyfin.org/posts/186 |
| 34 | open | Assign Users to TV Tuners | https://features.jellyfin.org/posts/461 |
| 34 | open | M3U management | https://features.jellyfin.org/posts/1005 |
| 28 | open | IPTV channel list / zapping-style view (2025-12) | https://features.jellyfin.org/posts/3678 |
| 24 | open | HDHomeRun TV guide API support | https://features.jellyfin.org/posts/190 |
| 18 | open | Hauppauge tuner support | https://features.jellyfin.org/posts/844 |
| 18 | open | Radio as a library type | https://features.jellyfin.org/posts/1896 |
| 17 | open | Scheduling priority for DVR series recordings | https://features.jellyfin.org/posts/36 |
| 17 | open | Edit Live TV channel order | https://features.jellyfin.org/posts/828 |
| 15 | open | Straight to Live TV Guide | https://features.jellyfin.org/posts/1831 |
| 15 | open | IPTV Catchup | https://features.jellyfin.org/posts/1918 |
| 15 | open | Sort XMLTV map channel list | https://features.jellyfin.org/posts/92 |
| 14 | open | Playback of in-progress DVR recordings | https://features.jellyfin.org/posts/1142 |
| 14 | open | Live TV default channel order | https://features.jellyfin.org/posts/1910 |
| 14 | open | Live TV home view: channel filtering + consistent logos (2025-12) | https://features.jellyfin.org/posts/3662 |
| 13 | open | Option "None" when mapping channels | https://features.jellyfin.org/posts/1651 |
| 11 | open | Keep current channel playing while viewing guide (2025-09) | https://features.jellyfin.org/posts/3468 |
| 11 | open | Xtream-Codes VOD/Series | https://features.jellyfin.org/posts/252 |
| 9 | open | Custom headers for IPTV M3U | https://features.jellyfin.org/posts/2491 |
| 8 | open | Rewind/Fast Forward Live TV | https://features.jellyfin.org/posts/1830 |
| 8 | open | Separate refresh for M3U and EPG (2025-09) | https://features.jellyfin.org/posts/3488 |
| 7 | open | Live TV categories/filters customizable | https://features.jellyfin.org/posts/1350 |
| 6 | open | Treat all Live TV viewing as temporary recording (timeshift) | https://features.jellyfin.org/posts/1950 |
| 6 | open | LiveTV logos from M3U instead of EPG | https://features.jellyfin.org/posts/2787 |
| 4 | open | Hide option for M3U/HDHomeRun channels | https://features.jellyfin.org/posts/1817 |
| 4 | open | Record ANY program on a specific channel | https://features.jellyfin.org/posts/2034 |
| 3 | open | Keep-up-to for series recordings | https://features.jellyfin.org/posts/214 |
| 3 | open | Recording post processing option | https://features.jellyfin.org/posts/311 |
| 3 | open | One live TV channel stream for multiple users | https://features.jellyfin.org/posts/1555 |
| 3 | open | Configurable TV channel priority for DVR (2025-02) | https://features.jellyfin.org/posts/3143 |

GitHub `jellyfin/jellyfin` label `live-tv`: 131 issues total, 27 open (2026-10-12).
Top by reactions: #13147 Schedules Direct broken (23), #6103 XMLTV cache purge (20),
#2548 channels persist after tuner removal (17), #10914 DVB subs (5).

---

## Ranked gaps (most user pain first)

### 1. Guide data: Schedules Direct keeps blocking Jellyfin; Emby ships free guide data — HIGH

- Schedules Direct's own home page (checked 2026-10-12) says: "All JellyFin (including 12.1!) ... have bugs talking to us. Your IP probably will be blocked." https://schedulesdirect.org/
- #13147 (2024-12-02, top-reacted live-tv issue, 23): SD disabled accounts because Jellyfin re-sent expired tokens and ignored error 4003; the SD developer estimated 500+ Jellyfin clients affected. Closed 2026-05-05 by PR #16220. https://github.com/jellyfin/jellyfin/issues/13147 , https://github.com/jellyfin/jellyfin/pull/16220
- 12.0 release notes claim "EPG now honors SchedulesDirect API error codes, preventing lockouts." https://jellyfin.org/posts/jellyfin-release-12.0
- But #18045 (2026-09-14, open, confirmed): 12.0 blocked again after ~24 h; "more than 10000 requests for images"; SD support says "the bug is still in the JF 12.0 application". https://github.com/jellyfin/jellyfin/issues/18045
- Emby user (2025-08-01): Jellyfin lacks a comparable guide because SD blocks it; "Emby is superior to both [Jellyfin, NextPVR] for Live TV." https://emby.media/community/topic/115266-thoughts-on-jellyfin/page/2
- Emby ships "Emby Guide Data", free (with Premiere) for US/Canada/UK, no third-party account. https://emby.media/support/articles/Emby-Guide-Data.html , https://emby.media/support/articles/Live-TV.html
- Jellyfin supports only Schedules Direct or XMLTV, one at a time per the setup guide. https://jellyfin.org/docs/general/server/live-tv/setup-guide
- Fider #190 HDHomeRun guide API (24 votes, open since 2019). https://features.jellyfin.org/posts/190
- XMLTV improvements in 12.0: imports background images and episode thumbnails, and skips unchanged programs on refresh. https://jellyfin.org/posts/jellyfin-release-12.0
- Deadfin note: SD rate limits (images: daily cap reset at 00:00Z; token refresh on 4003; stop on 4001/4004/4005/4008) are the parity-plus requirement. Do not copy upstream's behavior here.

### 2. Channel groups / categories / channel management — HIGH

- Fider #186 "IPTV Channel Groupings" is the top Live TV request by far: 377 votes, status "planned" since 2025-03-23, open since 2019. https://features.jellyfin.org/posts/186
- Emby imports M3U groups (`tvg-group`/group-title) as tags for guide management and parental filtering. https://emby.media/support/articles/M3U-Tuners.html
- Emby web guide shows group filter chips; the Android TV app has a tag-filter button (staff, 2025-07). https://emby.media/community/topic/148793-android-app-live-tv-guide-unable-to-navigate-categories/
- Emby 4.8.3 (2024-03): "M3U Tuner - Support restricting imported channels based on group" and "Support embedded xml guide data". Emby 4.7.3 (2022-06) fixed errors "when reordering live tv channels" (so reorder exists). https://forum.cloudron.io/topic/2609/emby-package-updates/36
- Emby channel management: checkboxes to exclude channels from import (staff, 2021). https://emby.media/community/topic/83136-live-tv-channels-editing-current-state
- Jellyfin: no native hide/reorder/renumber. 2025 forum threads: an 11,000-channel M3U user has no hide option; users told to use Dispatcharr or edit `tvg-chno`. https://forum.jellyfin.org/t-m3u-channel-filter?pid=64083 , https://forum.jellyfin.org/t-live-tv-channel-names?pid=65017 , https://forum.jellyfin.org/t-live-tv-sort-channels?pid=29300 , https://forum.jellyfin.org/t-editing-enabled-live-tv-channels
- Related Fider: #828 order (17), #1910 default order (14), #3662 channel filtering (14), #1651 "None" mapping (13), #1350 custom categories (7), #1817 hide (4).
- Jellyfin guide categories are fixed (Movies, Sports, Kids, News); one user reported the Sports filter went blank and favorites did not show. https://forum.jellyfin.org/t-recommended-iptv-setup-w-groups
- Closed: PR #12970 "Add ability to remove a ChannelMapping" (merged 2025-01-15, in 10.11). 10.11 also lists Live TV channels on the Favorites page. https://github.com/jellyfin/jellyfin/pull/12970 , https://jellyfin.org/posts/jellyfin-release-10.11.0

### 3. Client guide UI, especially Apple TV and Roku — HIGH (likely the community member's actual pain)

- Swiftfin issue #842 "Live TV Guide" open since 2023-09-14. A commenter: "I've since paid for Emby because of the live tv UI on ATV." Maintainer (2026-09-25): an EPG grid lands in Swiftfin 1.7 behind `Experimental > Live TV EPG`. https://github.com/jellyfin/Swiftfin/issues/842
- 2025-11 forum: Live TV does not appear on Swiftfin tvOS even with the experimental toggle; iOS works. https://forum.jellyfin.org/t-live-tv-on-appletv?pid=66691
- Swiftfin 1.6 (2026-08-12) added Live TV cleanup, posters, recordings. https://github.com/jellyfin/swiftfin/releases
- Emby for Apple TV has had a grid EPG since 2020. https://emby.media/community/topic/87175-emby-for-apple-tv-improved-playback-and-live-tv-guide/
- Roku: Jellyfin guide only reachable via the `*` key ("a little inconsistent", 2024-10). Open issues: guide "incredibly slow" (#598), "add TV Guide to OSD" (#1005), playback crashes (#597, #794). https://forum.jellyfin.org/t-is-there-a-way-to-get-to-the-livetv-guide-from-the-roku-client , https://github.com/jellyfin/jellyfin-roku/issues/598 , https://github.com/jellyfin/jellyfin-roku/issues/1005
- Emby for Roku: full graphical guide, mini-guide during playback, record/cancel from the mini-guide. https://emby.media/more-roku-osg.html , https://emby.media/emby-for-roku.html
- Emby Android TV: numeric channel entry, quick program changer during playback, previous-channel button. https://emby.media/community/topic/50878-channel-surfing
- Jellyfin requests: keep the channel playing while browsing the guide (#3468, 11), zapping list (#3678, 28), straight to guide (#1831, 15).
- Deadfin note: these are client gaps (deadfin-web, Swiftfin, Roku). The server must serve the guide APIs fast for large lineups (Tizen guide scroll bug, jellyfin-web #7198).

### 4. Tuner handling: stream limits, sharing, release, transcode bloat — HIGH/MEDIUM

- #10996 "m3u live TV simultaneous stream limit not honored": open since 2024-02-11, confirmed; reporter says "still an issue in 12.1" (2026-09-27). https://github.com/jellyfin/jellyfin/issues/10996
- Core-team statement: each Jellyfin client stream opens its own provider connection, even for the same channel (forum). https://forum.jellyfin.org/t-livetv-not-working-with-multiple-streams
- 2025-08 forum: second viewer on another channel kicks the first viewer off, with no error. Same source.
- 10.10 (PR #12529, 2024-09): per-tuner toggles for HTTP stream sharing, fMP4, fallback bitrate. Stream sharing applies only to MPEG-TS-looking URLs (PR #11805, 10.9.x). https://github.com/jellyfin/jellyfin/pull/12529 , https://github.com/jellyfin/jellyfin/pull/11805
- Tuners stay locked after playback (#15769, 10.11.4); fixed by PR #17178 (merged 2026-06-27). https://github.com/jellyfin/jellyfin/issues/15769 , https://github.com/jellyfin/jellyfin/pull/17178
- Live remux fills the transcode directory until the disk is full: #16728 (closed), #17120 (open 2026-06). https://github.com/jellyfin/jellyfin/issues/16728 , https://github.com/jellyfin/jellyfin/issues/17120
- 12.x regressions: HDHomeRun recordings transcode on playback (#18055); HDHomeRun "fatal player error" after upgrade (#18297). https://github.com/jellyfin/jellyfin/issues/18055 , https://github.com/jellyfin/jellyfin/issues/18297
- 12.0: HDHomeRun now direct plays; M3U defaults to remux (PR #17682). https://jellyfin.org/posts/jellyfin-release-12.0 , https://github.com/jellyfin/jellyfin/pull/17682
- Emby: viewers of the same channel share one tuner stream; distinct channels each use a tuner. https://emby.media/community/topic/93980-how-many-devices-does-hdhomerun-support , https://emby.media/community/topic/94870-live-tv-limit-based-on-available-tuners
- Emby is not perfect: a 4.10.0.40 bug lets duplicate recording consumers hold hidden M3U connections. https://emby.media/community/topic/150036-live-tv-can-consume-hidden-tunersconnections-due-to-duplicate-recording-consumers/
- Fider #461 assign users to tuners (34), #2223 per-tuner permission (5).

### 5. Time-shifting, pause/rewind, in-progress recordings — MEDIUM

- Jellyfin: pause works; rewind and fast-forward after pause do not on Android TV (2026-01); cannot start an in-progress recording from the beginning (10.10.7 + HDHomeRun). https://forum.jellyfin.org/t-livetv-playback-on-androidtv , https://forum.jellyfin.org/t-how-to-watch-live-tv-show-that-is-recording-from-beigining
- Roku: "with direct play there's no way to go back since the server is not buffering anything." https://forum.jellyfin.org/t-can-t-live-tv-timeshift-using-the-roku-client?pid=15082
- Fider #1142 (14), #1830 (8), #1950 (6).
- Emby: an admin "Live Stream Buffer" setting; timeshift supported, with bugs on some clients. https://emby.media/support/articles/DVR-Settings.html , https://emby.media/community/topic/97186-increase-live-tv-pause-buffer , https://emby.media/community/topic/90244-timeshift-issues/
- Emby 4.8.9 (2024-09): "Add prompt for playback when a channel is recording" (watch from start). https://forum.cloudron.io/topic/2609/emby-package-updates/36
- Plex has a "Time Shift" feature for Plex Pass users. https://techcrunch.com/2017/11/27/plexs-dvr-now-lets-you-skip-the-commercials-by-removing-them-for-you
- Deadfin note: a server-side ring buffer per shared tuner stream (HLS window) gives timeshift to every client and covers gap 4 at the same time.

### 6. Commercial detection / skipping — MEDIUM

- Neither has comskip built in. Channels DVR bundles comskip and marks skip points. Plex detects commercials and can optionally cut them out. https://community.getchannels.com/t/playback-is-not-skipping-commercials/20689 , https://support.plex.tv/?p=39084
- Emby: third-party ComSkipper plugin reads comskip `.edl` and seeks during playback (v2.2.0.0, 2025-10). https://github.com/BillOatmanWork/Emby.ComSkipper , https://emby.media/community/topic/143255-comskipper-issues
- Users report skipping in-progress recordings works reliably on Emby Android (2025-08). https://emby.media/community/topic/115266-thoughts-on-jellyfin/page/2
- The Emby Android TV app ignores ComSkipper seeks (4.10, 2025/26). https://emby.media/community/topic/149695-android-tv-app-ignores-remote-playback-commands-comskipper-cannot-seek/
- Built-in comskip is requested on Emby; no staff commitment (2024). https://emby.media/community/topic/124845-built-in-ad-commercial-skipping
- Jellyfin does not read EDL. The path is comskip → comchap/ffmpeg chapters → Chapter Segments Provider → media segments (10.10+) → "Skip" button. A user got this working in 2025-03, but it needs remuxing and a manual segment scan. https://forum.jellyfin.org/t-comskip-and-media-segments , https://forum.jellyfin.org/t-how-do-i-use-comskip?pid=66868
- Jellyfin docs mention comskip only as an idea to explore. https://jellyfin.org/docs/general/server/live-tv/post-process
- Release closure: 10.10 (2024-10) added Media Segments, which is the hook. https://alternativeto.net/news/2024/10/jellyfin-10-10-released-with-enhanced-transcoding-new-media-segments-ffmpeg-7-0-and-more/
- Deadfin opportunity: an EDL-to-media-segment provider for recordings (Commercial segment type), with optional comskip run after recording.

### 7. Series recording rules and conflicts — MEDIUM

- Jellyfin has KeepUpTo, RecordNewOnly, pre/post padding, and "skip if in library" (`SeriesTimerInfo`). https://git.sudo.is/mirrors/jellyfin/src/branch/master/MediaBrowser.Controller/LiveTv/SeriesTimerInfo.cs
- Jellyfin bugs: "new only" ignores the XMLTV `<new/>` tag (forum); "already in library" not respected (#13637, closed 2025-02); a guide refresh wiped schedules (#12979, closed 2024-11). Padding cannot be negative. https://forum.jellyfin.org/printthread.php?tid=12478 , https://github.com/jellyfin/jellyfin/issues/13637 , https://github.com/jellyfin/jellyfin/issues/12979 , https://forum.jellyfin.org/t-dvr-recording-offset-due-to-live-stream-delay
- Emby extras:
  - **Keyword recording** (4.7.7, 2022): record every matching program on any channel. https://emby.media/community/index.php?/blogs/entry/543-emby-server-477-keyword-recording-lock-screen%C2%A0mini-dock-more/
  - "check all libraries for existing episodes" (4.8.4, 2024-04). https://forum.cloudron.io/topic/2609/emby-package-updates/36
  - Per-series padding overrides. https://emby.media/community/topic/71736-how-to-add-time-to-recordings
  - Record on this channel or any channel. https://emby.media/community/topic/91184-dvr-recording-question/
- Neither has priority or conflict resolution. Emby staff: "we don't currently have this, but it's planned" (2019). Emby: "doesn't currently have any conflict resolution". https://emby.media/community/topic/67818-prioritizing-recordings/ , https://emby.media/community/topic/45862-recording-conflictreporting
- Jellyfin Fider #36 (17), #3143 (3), #2034 keyword-ish (4), #214 keep-up-to (3).
- Channels DVR and Tablo have pass priority. https://community.getchannels.com/t/need-some-guidance-with-pass-priority/10570
- Deadfin opportunity: rule priority plus an up-front conflict report (a tuner-count-aware scheduler). Neither competitor does this well.

### 8. Channel logos and mapping — MEDIUM/LOW

- Jellyfin: logos stuck after remap (#15748, closed 2025-12). PR #10841 refreshes the image on remap (merged 2026-06). PR #16914 re-fetched icons on every guide refresh, then PR #17549 reverted it (2026-08). https://github.com/jellyfin/jellyfin/issues/15748 , https://github.com/jellyfin/jellyfin/pull/17549
- Fider #2787: logos from M3U instead of EPG (6). #424: logo directory (6). jellyfin-web #7383: default to channel logos (10 reactions). https://github.com/jellyfin/jellyfin-web/issues/7383
- Emby: per-tuner option to take channel images from guide data or from the M3U. https://emby.media/support/articles/M3U-Tuners.html
- Emby 4.7.3: "Improve automatic channel mapping with xml tv". https://forum.cloudron.io/topic/2609/emby-package-updates/36

### 9. Multiple tuner sources, failover, priority — LOW/MEDIUM

- Neither merges the same channel across sources with failover. Emby staff: "emby does not group the same channels across multiple providers and probably wont". HDHomeRun units are pooled (any free tuner). https://emby.media/community/topic/48492-live-tv-m3u-sources , https://emby.media/community/topic/128161-iptv-fallover-fallback
- Jellyfin users see duplicates with two M3U providers. https://forum.jellyfin.org/t-tv-guide-uk?pid=39292
- Workarounds: Dispatcharr, Threadfin, hdhriptv (Go; shared upstream plus failover). https://pkg.go.dev/github.com/arodd/hdhriptv

### 10. IPTV specifics: Xtream Codes, catchup, headers — LOW/MEDIUM

- Neither has native Xtream support.
  - Emby staff: will not integrate. https://emby.media/community/topic/116732-question/
  - Community plugins exist for Jellyfin (Kevinjil/Jellyfin.Xtream, 317 stars) and Emby (Xtream Tuner, XC2EMBY). https://github.com/Kevinjil/Jellyfin.Xtream , https://www.jellywatch.app/hub/emby-xtream-tuner-imp
  - Security caveat: Xtream URLs embed credentials. https://forum.jellyfin.org/t-jellyfin-xtream?pid=6576
- Emby M3U tuner has Referer/User-Agent options and `tvg-shift`. Jellyfin has User-Agent only. Fider #2491 custom headers (9), #3871 EXTVLCOPT (3). https://emby.media/support/articles/M3U-Tuners.html
- Catchup: Fider #1918 (15). The firestaerter3 plugin claims catchup. https://github.com/firestaerter3/Jellyfin-Xtream-Library

### 11. Post-processing and conversion — LOW (near parity)

- Both run a post-processing script with the file path after a recording. https://jellyfin.org/docs/general/server/live-tv/post-process , https://emby.media/support/articles/DVR-Settings.html
- Emby adds "Automatically Convert Content" (a Premiere feature; a convert job on a folder watches new recordings). https://emby.media/premiere.html , https://emby.media/community/topic/70016-live-tv-recordings-to-mp4
- Jellyfin: script stdout is not logged, and quoting is hard (from the docs).

### 12. Remote access / "cloud" DVR — LOW (parity)

- Neither has a cloud DVR. Both schedule server-side from any client.
- Jellyfin 12.0 fixed Live TV returning unreachable "server-local" stream URLs to clients (PR #17298), which broke remote/proxied playback. https://github.com/jellyfin/jellyfin/pull/17298

### 13. HDHomeRun quirks, ATSC 3.0 / DRM — LOW (nobody solves DRM)

- DRM-encrypted ATSC 3.0 does not work in Plex/Emby/Jellyfin. SiliconDust: DRM "cannot be supported on any Apple product... any Microsoft product... any web based television". The FLEX 4K falls back to the ATSC 1.0 version. https://forum.silicondust.com/forum/viewtopic.php?p=404631 , https://www.silicondust.com/
- AC-4 audio on ATSC 3.0 is a separate codec problem. https://forums.plex.tv/t/atsc3-0-broadcast-and-ac-4-sound/647437?page=19
- Legacy HDHomeRun: Jellyfin supports legacy units after a Windows channel scan. Emby requires a "DLNA 2015+ model". **Jellyfin advantage.** https://jellyfin.org/docs/general/server/live-tv/setup-guide , https://emby.media/support/articles/Live-TV.html
- Jellyfin "Allow hardware transcoding" uses HDHomeRun transcoding models (EXTEND). Docker needs host networking (changing UDP port). https://jellyfin.org/docs/general/server/live-tv/
- Hauppauge: Emby supports it on Windows (WinTV 8.5+). Jellyfin does not (Fider #844, 18).

### 14. Radio — LOW

- Both put audio-only M3U streams under Live TV. Jellyfin docs need a local M3U with `#EXTINF`. Fider #1896 (18), #2054 (12). Emby has an open request since 2016. https://jellyfin.org/docs/general/server/live-tv/internet-radio , https://emby.media/community/topic/38469-live-tv-separate-radio

### 15. Licensing (Jellyfin/Deadfin advantage)

- Emby Live TV/DVR needs Premiere: $4.99/mo, $54/yr, $119 lifetime. https://emby.media/premiere.html , https://emby.media/support/articles/Live-TV.html

### Jellyfin releases that closed (or narrowed) gaps

| Release | Date | Live TV change |
|---|---|---|
| 10.9.x | 2024-05/06 | More compatible LiveTV transcode profile (#11801); SharedStream only for MPEG-TS (#11805); relaxed remux (#11851) |
| 10.10.0 | 2024-10-27 | Per-tuner compatibility profiles: stream sharing toggle, fMP4, fallback bitrate (#12529); Media Segments (commercial-skip hook) |
| 10.11.0 | 2025-10-20 | Live TV channels on Favorites page; M3U native framerate; remove channel mapping (#12970) |
| 12.0 | 2026-09-07 | SD error-code handling (#16220, but see #18045); HDHomeRun direct play (#17682); M3U defaults to remux; XMLTV images; no server-local stream URLs (#17298); tuner release fix (#17178) |
| Swiftfin 1.7 | 2026 (pending) | Experimental tvOS/iOS EPG grid |

Still open after 12.1: stream limit (#10996), SD lockout (#18045), channel groups (#186), timeshift, priority/conflicts, transcode-dir growth (#17120).

---

## Test sources (free, legal)

All checked to respond on 2026-10-12 unless noted.

**M3U playlists**

- iptv-org/iptv (140k stars): only publicly available streams. https://github.com/iptv-org/iptv
  - Whole list: https://iptv-org.github.io/iptv/index.m3u
  - By country: https://iptv-org.github.io/iptv/countries/us.m3u
  - By category: https://iptv-org.github.io/iptv/categories/news.m3u
  - Includes many Pluto/Samsung FAST channels.
- Free-TV/IPTV (21k stars): curated free-to-air channels only. https://raw.githubusercontent.com/Free-TV/IPTV/master/playlist.m3u8
- The LegalStream sample playlist in the Jellyfin docs is deprecated (2022). https://jellyfin.org/docs/general/server/live-tv/setup-guide

**HLS test streams (deterministic)**

| Source | Type | URL |
|---|---|---|
| Apple bipbop advanced | VOD, multi-variant TS | https://devstreaming-cdn.apple.com/videos/streaming/examples/img_bipbop_adv_example_ts/master.m3u8 |
| Apple bipbop 16x9 | VOD | https://devstreaming-cdn.apple.com/videos/streaming/examples/bipbop_16x9/bipbop_16x9_variant.m3u8 |
| Mux | VOD | https://test-streams.mux.dev/x36xhzz/x36xhzz.m3u8 |
| Akamai | live test | https://cph-p2p-msl.akamaized.net/hls/live/2000341/test/master.m3u8 |
| Unified Streaming | live with SCTE-35 ad markers (useful for commercial-marker work) | https://demo.unified-streaming.com/k8s/live/stable/scte35.isml/.m3u8 |

Apple's example list: https://developer.apple.com/streaming/examples/

**XMLTV guides and samples**

- iptv-org/epg (grabber toolkit; generate your own XMLTV). https://github.com/iptv-org/epg
- XMLTV format spec and DTD: https://wiki.xmltv.org/index.php/XMLTVFormat , https://raw.githubusercontent.com/XMLTV/xmltv/master/xmltv.dtd

**Self-generated channels (best for deterministic tests)**

ErsatzTV and Tunarr build live channels from your own media and expose HDHomeRun plus XMLTV endpoints.

- ErsatzTV (3k stars). https://github.com/ErsatzTV/legacy
- Tunarr (2.6k stars). https://github.com/chrisbenincasa/tunarr

## HDHomeRun emulators / M3U proxies

| Tool | Stars | Status | Note |
|---|---|---|---|
| Dispatcharr | 4.2k | active 2026 | M3U/EPG manager, HDHR output. Note: SD lists Dispatcharr ≤0.27.2 as buggy too. https://github.com/Dispatcharr/Dispatcharr |
| Threadfin | 1.7k | xTeVe fork, last push 2025-10 | https://github.com/Threadfin/Threadfin |
| xTeVe | — | dormant since 2021 | https://github.com/xteve-project/xTeVe-Downloads |
| hdhriptv | 10 | Go, 2026 | Shared upstream, stall detection, failover; close to Deadfin's needs. https://github.com/arodd/hdhriptv |
| antennas | 374 | Tvheadend→HDHR, last push 2023 | https://github.com/jfarseneau/antennas |
| tvhProxy | 224 | archived | https://github.com/jkaberg/tvhProxy |
| locast2plex | 229 | dead | Locast shut down in 2021. https://github.com/tgorgdotcom/locast2plex |
| IPTVBoss | — | paid | Windows/Java M3U/EPG editor; no HDHR emulation found in this research |

## Real hardware (SiliconDust, prices 2026-10-12)

Source: https://www.silicondust.com/ and https://shop.silicondust.com/shop/product-category/refurbished-hardware

| Model | Price | Tuners | ATSC 3.0 | Note |
|---|---|---|---|---|
| HDHomeRun FLEX DUO | $109.99 | 2 × ATSC 1.0 | no | |
| HDHomeRun FLEX QUATRO | $149.99 | 4 × ATSC 1.0 | no | |
| HDHomeRun FLEX 4K | $199.99 ($174.99 refurbished) | 4 | on 2 of the 4 tuners | DRM channels do not work |
| SCRIBE 4K | $279.99 | 4 (same as FLEX 4K) | yes | Internal 1 TB drive; replaces SCRIBE QUATRO. https://www.pcworld.com/article/624370/hdhomerun-flex-4k-review.html |
| HDFX-4K-DEV | $299.99 | | | Developer firmware |

Recommendation for Deadfin testing: one FLEX DUO (cheapest real ATSC 1.0 MPEG-2) plus one FLEX 4K (ATSC 3.0/HEVC/AC-4 edge cases).

**Schedules Direct:** US$35 per year, US$9 for 2 months, 7-day free trial; US/Canada. https://schedulesdirect.org/ . The SiliconDust DVR guide is also $35/yr.

**Emby Premiere** (for comparison): $54/yr or $119 lifetime. https://emby.media/premiere.html

## Anchor use case: local channels over the internet (checked 2026-10-12)

Facts behind `livetv.md` §0. Marked **[U]** where only a forum or a single
secondary report supports the claim.

### FAST and station sources of local news

- CBS News local streams, 14 markets (2023): https://www.cbsnews.com/boston/news/wbz-launches-local-streaming-news-network-cbsn-boston/ ;
  on Pluto TV nationally: https://www.paramountpressexpress.com/cbs-news-and-stations/releases/view?id=53538
- Plex adds NBC and Telemundo owned-station local news (March 2025): https://thedesk.net/2025/03/plex-adds-more-nbc-free-streaming-channels/
- FOX LOCAL, station by location: https://www.fox2detroit.com/streaming-lions-fox-2
- NewsON, about 200 stations: https://en.wikipedia.org/wiki/NewsON
- Local Now, over 225 markets: https://cordcuttersnews.com/local-now-expands-free-streaming-app/
- Samsung TV Plus local news: https://www.tvtechnology.com/news/4k-fireplace-more-local-news-channels-coming-to-samsung-tv-plus
- Scripps News, streaming-only since November 2024: https://en.wikipedia.org/wiki/Scripps_News
- Terms: Pluto's terms forbid access the service does not authorize, as
  reported at https://torrentfreak.com/?p=231455 ; the MPA takedown of a
  Pluto M3U repository: https://tech.slashdot.org/story/23/02/28/2057203/
- No service documents its tokens, geo checks or ad markers. **[U]**

### HLS, SSAI and Apple players

- Apple HLS Authoring Specification:
  https://developer.apple.com/documentation/http-live-streaming/hls-authoring-specification-for-apple-devices
  - 1.1: video is H.264, HEVC, Dolby Vision or AV1, with no MPEG-2;
  - 1.5: HEVC in fMP4;
  - 1.14: deinterlace interlaced content;
  - 2.5: multichannel audio is AC-3, E-AC-3 or E-AC-3 JOC, with no AC-4;
  - 8.13, 8.15, 8.16, 8.17: discontinuity rules;
  - 8.19: interstitials in `EXT-X-DATERANGE`.
- SSAI stitchers replace `EXT-X-CUE-OUT`/`-IN` with discontinuities. SCTE-35
  in HLS goes in `EXT-X-DATERANGE` `SCTE35-OUT`/`-IN`:
  - https://docs.aws.amazon.com/mediatailor/latest/ug/manifest-hls-example.md
  - https://docs.cloud.google.com/video-stitcher/docs/concepts/ad-markers
- AVPlayer discontinuity failures, from Apple forum reports:
  - mismatched discontinuity sequences stall: https://developer.apple.com/forums/thread/792465
  - audio and video discontinuities that differ give a black screen: https://developer.apple.com/forums/thread/678253
- Apple TV freezes during Pluto ad breaks in Channels **[U]**:
  - https://community.getchannels.com/t/pluto-tv-on-screen-message-pluto-tv-is-longer-available-on-this-device/45844?page=5
  - https://community.getchannels.com/t/odd-live-playback-of-pluto-channels-behavior/42882
- Expiring M3U URLs:
  - Channels DVR refreshes on a schedule, and users script the refresh API: https://community.getchannels.com/t/auto-refresh-m3u-url/36521
  - Emby refreshes only with the guide: https://emby.media/community/topic/74370-refresh-q/

### Remote HDHomeRun, ATSC 3.0, DRM

- Prices: shop.silicondust.com lists the FLEX 4K at $199.99, FLEX QUATRO at
  $149.99 and FLEX DUO at $109.99. The DUO and QUATRO are ATSC 1.0 only.
  The FLEX 4K has 4 tuners, 2 of them ATSC 3.0.
  - https://shop.silicondust.com/shop/
  - https://www.pcworld.com/article/624370/hdhomerun-flex-4k-review.html
- `?transcode=` works on EXTEND models only. The lineup marks DRM
  channels. https://info.hdhomerun.com/info/http_api
- AC-4:
  - SiliconDust's app converts AC-4 to AC-3 in its cloud: https://blog.lon.tv/2022/07/24/i-got-free-tv-over-the-air-with-atsc-3-0/
  - FFmpeg has no AC-4 decoder. Ticket #8349 is stalled: https://ffmpeg.org/pipermail/ffmpeg-trac/2025-July/074054.html
  - Plex has no AC-4: https://forums.plex.tv/t/atsc3-0-broadcast-and-ac-4-sound/647437
- A3SA DRM:
  - The 2024 reports say Apple TV and most Roku players cannot play it: https://cordcuttersnews.com/hdhomeruns-atsc-3-0-nextgen-tv-drm-wont-work-on-most-roku-players-and-apple-tvs/
  - SiliconDust asked the FCC to stop broadcasters from encrypting (July 2026): https://www.tvtechnology.com/platform/broadcast/hdhomerun-enables-operation-during-an-internet-outage
  - The FCC 2025 NextGen TV FNPRM: https://www.commlawcenter.com/2025/10/a-simulcast-sunset-on-the-horizon-as-fcc-adopts-nextgen-tv-nprm.html
- Remote viewing:
  - SiliconDust's view of streams over a VPN, reported second-hand: https://community.getchannels.com/t/home-vpn-server/229
  - ATSC 3.0 runs at about half the ATSC 1.0 bitrate: https://blog.lon.tv/2023/04/16/streaming-atsc-3-content-outside-the-home/
  - Typical HD bitrates of 10–15 Mbit/s. **[U]**
- Guide:
  - HDHomeRun XMLTV for third-party apps needs a paid DVR guide subscription and DeviceAuth: https://info.hdhomerun.com/info/dvr:xmltv
  - Schedules Direct pricing: check schedulesdirect.org before quoting it. **[U]**

### Locast

- On 31 August 2021, Judge Stanton (S.D.N.Y.) held that Locast's donation
  requests were charges above cost recovery, so the §111(a)(5) exemption
  did not apply. Locast suspended on 2 September 2021, and a permanent
  injunction followed.
  - https://thedesk.net/2021/09/locast-stops-streaming-suspends-broadcast-tv/
  - https://stopthecap.com/2021/09/16/judge-orders-permanent-injunction-that-likely-deals-final-death-blow-to-locast
