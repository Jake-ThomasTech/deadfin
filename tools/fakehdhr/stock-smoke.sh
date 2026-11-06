#!/usr/bin/env bash
# Point the pinned stock Jellyfin 12.0.0 image at fakehdhr and exercise the
# Live TV path end to end: tuner add, XMLTV guide refresh, channel list, live
# open + HLS transcode segment, tuner-limit behavior, close, and a one-program
# recording. Spike evidence for docs/spikes/livetv.md; not a gate.
#
# Usage: tools/fakehdhr/stock-smoke.sh   (needs docker, ffmpeg, curl, python3)
#
# TARGET runs another image, for example a Deadfin build. TUNER selects the
# tuner: hdhomerun (default), m3u (/playlist.m3u) or lineup (/lineup.m3u,
# the HDHomeRun name for the same playlist). PLAYBACK=0 stops after the
# channel list.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
image=${TARGET:-$(awk '/image: jellyfin\/jellyfin@/ {print $2; exit}' "$root/tools/goldenrec/compose.yaml")}
tuner=${TUNER:-hdhomerun}
playback=${PLAYBACK:-1}
name=fakehdhr-stock-smoke
J=http://127.0.0.1:18096
H='Content-Type: application/json'

cleanup() {
	docker rm -f "$name" >/dev/null 2>&1 || true
	[[ -n ${fake_pid:-} ]] && kill "$fake_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

go build -o "$work/fakehdhr" "$root/tools/fakehdhr"
"$work/fakehdhr" -listen 0.0.0.0:15004 -advertise http://host.docker.internal:15004 -discovery "" -tuners 2 -overlay=false >"$work/fake.log" 2>&1 &
fake_pid=$!

docker run -d --name "$name" -p 18096:8096 --add-host host.docker.internal:host-gateway "$image" >/dev/null
# /System/Info/Public answers before the startup wizard routes do.
for _ in $(seq 90); do
	curl -sf -X POST "$J/Startup/Configuration" -H "$H" \
		-d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}' 2>/dev/null && break
	sleep 1
done

# Throwaway credentials for the throwaway container.
curl -sf "$J/Startup/User" >/dev/null
curl -sf -X POST "$J/Startup/User" -H "$H" -d '{"Name":"smoke","Password":"smoke"}'
curl -sf -X POST "$J/Startup/Complete"
tok=$(curl -sf -X POST "$J/Users/AuthenticateByName" -H "$H" \
	-H 'Authorization: MediaBrowser Client="smoke", Device="curl", DeviceId="smoke1", Version="1"' \
	-d '{"Username":"smoke","Pw":"smoke"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["AccessToken"])')
A="Authorization: MediaBrowser Token=\"$tok\""
js() { python3 -c "import sys,json;d=json.load(sys.stdin);$1"; }

echo "== tuner + guide"
case $tuner in
hdhomerun) host='{"Type":"hdhomerun","Url":"http://host.docker.internal:15004"}' ;;
m3u) host='{"Type":"m3u","Url":"http://host.docker.internal:15004/playlist.m3u"}' ;;
lineup) host='{"Type":"m3u","Url":"http://host.docker.internal:15004/lineup.m3u"}' ;;
*) echo "TUNER must be hdhomerun, m3u or lineup" >&2; exit 2 ;;
esac
curl -sf -X POST "$J/LiveTv/TunerHosts" -H "$A" -H "$H" -d "$host" | js 'print(d["Type"],d.get("DeviceId") or d.get("Url"))'
curl -sf -X POST "$J/LiveTv/ListingProviders?validateListings=true" -H "$A" -H "$H" \
	-d '{"Type":"xmltv","Path":"http://host.docker.internal:15004/xmltv.xml","EnableAllTuners":true}' | js 'print(d["Type"],d["Id"])'
task=$(curl -sf "$J/ScheduledTasks" -H "$A" | js 'print([t["Id"] for t in d if t["Key"]=="RefreshGuide"][0])')
curl -sf -X POST "$J/ScheduledTasks/Running/$task" -H "$A" || true
for _ in $(seq 60); do
	[[ $(curl -sf "$J/ScheduledTasks/$task" -H "$A" | js 'print(d["State"])') == Idle ]] && break
	sleep 1
done
curl -sf "$J/LiveTv/Channels?addCurrentProgram=true" -H "$A" | tee "$work/channels.json" |
	js '[print(c["ChannelNumber"],c["Name"],(c.get("CurrentProgram") or {}).get("Name")) for c in d["Items"]]'
chan() { js "print([c['Id'] for c in d['Items'] if c['ChannelNumber']=='$1'][0])" <"$work/channels.json"; }
[[ $playback == 1 ]] || exit 0

profile='{"AutoOpenLiveStream":true,"IsPlayback":true,"DeviceProfile":{"TranscodingProfiles":[{"Container":"ts","Type":"Video","VideoCodec":"h264","AudioCodec":"aac","Protocol":"hls","MaxAudioChannels":"2"}]}}'
open_live() { curl -s -X POST "$J/Items/$1/PlaybackInfo" -H "$A" -H "$H" -d "$profile"; }

echo "== live open + transcode (2.1, interlaced MPEG-2 + AC-3 5.1)"
open_live "$(chan 2.1)" >"$work/pbi.json"
js 'm=d["MediaSources"][0];print(m["Path"],m["IsInfiniteStream"],m["LiveStreamId"][:16]);print(m["TranscodingUrl"])' <"$work/pbi.json"
url=$(js 'print(d["MediaSources"][0]["TranscodingUrl"])' <"$work/pbi.json")
item=$(echo "$url" | cut -d/ -f3)
main=$(curl -sf "$J$url" | grep -v '^#' | head -1)
seg=$(curl -sf "$J/videos/$item/$main" | grep -v '^#' | head -1)
curl -sf -o "$work/seg.ts" "$J/videos/$item/$seg"
ffprobe -v error -show_entries stream=codec_name,width,height,field_order -of csv=p=0 "$work/seg.ts"

echo "== tuner limit: 2 tuners, open 4.1 then 5.1"
open_live "$(chan 4.1)" >"$work/pbi41.json"
open_live "$(chan 5.1)" >"$work/pbi51.json"
# Upstream swallows the tuner's 503: PlaybackInfo still returns a source whose
# buffer file never appears, and the probe fails afterwards.
js 'print("5.1 sources:",len(d.get("MediaSources",[])),"ErrorCode:",d.get("ErrorCode"))' <"$work/pbi51.json"
curl -sf http://127.0.0.1:15004/status.json; echo
docker logs "$name" 2>&1 | grep -m2 -E 'Could not find file|Error probing live tv stream' || true

echo "== close"
for f in pbi pbi41 pbi51; do
	id=$(js 'print(d["MediaSources"][0]["LiveStreamId"])' <"$work/$f.json" 2>/dev/null) || continue
	curl -s -o /dev/null -w "$f close=%{http_code}\n" -X POST "$J/LiveStreams/Close?liveStreamId=$id" -H "$A"
done

echo "== record the airing program on 4.1"
pid=$(curl -sf "$J/LiveTv/Programs?channelIds=$(chan 4.1)&isAiring=true" -H "$A" | js 'print(d["Items"][0]["Id"])')
curl -sf "$J/LiveTv/Timers/Defaults?programId=$pid" -H "$A" >"$work/timer.json"
curl -sf -X POST "$J/LiveTv/Timers" -H "$A" -H "$H" -d @"$work/timer.json"
sleep 8
curl -sf "$J/LiveTv/Timers" -H "$A" | js '[print(t["Name"],t["Status"]) for t in d["Items"]]'
docker exec "$name" sh -c 'find /config/data/livetv/recordings -type f | head'
