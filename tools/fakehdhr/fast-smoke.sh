#!/usr/bin/env bash
# Point the pinned stock Jellyfin 12.0.0 image at fakehdhr's FAST-style HLS
# channels through the M3U tuner and observe: the input path upstream picks
# for an .m3u8 source, the live transcode across an SSAI ad break (800.2:
# discontinuity, new resolution and audio layout), and a tune of an expired
# token URL (800.4, 120 s tokens). Spike evidence for docs/spikes/livetv.md
# §5.3; not a gate.
#
# Usage: tools/fakehdhr/fast-smoke.sh   (needs docker, ffmpeg, curl, python3)
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
image=${TARGET:-$(awk '/image: jellyfin\/jellyfin@/ {print $2; exit}' "$root/tools/goldenrec/compose.yaml")}
name=fakehdhr-fast-smoke
J=http://127.0.0.1:18097
H='Content-Type: application/json'

cleanup() {
	docker rm -f "$name" >/dev/null 2>&1 || true
	[[ -n ${fake_pid:-} ]] && kill "$fake_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

go build -o "$work/fakehdhr" "$root/tools/fakehdhr"
"$work/fakehdhr" -listen 0.0.0.0:15014 -advertise http://host.docker.internal:15014 -discovery "" \
	-no-fault-channels -cache-dir "$work/cache" >"$work/fake.log" 2>&1 &
fake_pid=$!
t_fake=$(date +%s)

docker run -d --name "$name" -p 18097:8096 --add-host host.docker.internal:host-gateway "$image" >/dev/null
for _ in $(seq 90); do
	curl -sf -X POST "$J/Startup/Configuration" -H "$H" \
		-d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}' 2>/dev/null && break
	sleep 1
done
# Throwaway credentials for the throwaway container.
curl -sf "$J/Startup/User" >/dev/null
curl -sf -X POST "$J/Startup/User" -H "$H" -d '{"Name":"fast","Password":"fast"}'
curl -sf -X POST "$J/Startup/Complete"
tok=$(curl -sf -X POST "$J/Users/AuthenticateByName" -H "$H" \
	-H 'Authorization: MediaBrowser Client="fast", Device="curl", DeviceId="fast1", Version="1"' \
	-d '{"Username":"fast","Pw":"fast"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["AccessToken"])')
A="Authorization: MediaBrowser Token=\"$tok\""
js() { python3 -c "import sys,json;d=json.load(sys.stdin);$1"; }

echo "== M3U tuner + XMLTV"
curl -sf -X POST "$J/LiveTv/TunerHosts" -H "$A" -H "$H" \
	-d '{"Type":"m3u","Url":"http://host.docker.internal:15014/playlist.m3u","TunerCount":0}' | js 'print(d["Type"])'
curl -sf -X POST "$J/LiveTv/ListingProviders" -H "$A" -H "$H" \
	-d '{"Type":"xmltv","Path":"http://host.docker.internal:15014/xmltv.xml","EnableAllTuners":true}' | js 'print(d["Type"])'
t_m3u=$(date +%s)
task=$(curl -sf "$J/ScheduledTasks" -H "$A" | js 'print([t["Id"] for t in d if t["Key"]=="RefreshGuide"][0])')
for _ in $(seq 30); do [[ $(curl -sf "$J/ScheduledTasks/$task" -H "$A" | js 'print(d["State"])') == Running ]] && break; sleep 1; done
while [[ $(curl -sf "$J/ScheduledTasks/$task" -H "$A" | js 'print(d["State"])') != Idle ]]; do sleep 1; done
curl -sf "$J/LiveTv/Channels" -H "$A" >"$work/channels.json"
js 'print(len(d["Items"]),"channels;",[c["Name"] for c in d["Items"] if c["Name"].startswith("FAST")])' <"$work/channels.json"
chan() { js "print([c['Id'] for c in d['Items'] if c['Name']=='$1'][0])" <"$work/channels.json"; }

profile='{"AutoOpenLiveStream":true,"IsPlayback":true,"DeviceProfile":{"TranscodingProfiles":[{"Container":"ts","Type":"Video","VideoCodec":"h264","AudioCodec":"aac","Protocol":"hls","MaxAudioChannels":"2"}]}}'
open_live() { curl -s -X POST "$J/Items/$1/PlaybackInfo" -H "$A" -H "$H" -d "$profile"; }

echo "== 800.2 SSAI: open, then play across the ad break at +120 s"
open_live "$(chan FAST-SSAI)" >"$work/pbi.json"
js 'm=d["MediaSources"][0];print("path",m["Path"]);print("SupportsDirectStream",m["SupportsDirectStream"],"IsInfiniteStream",m["IsInfiniteStream"]);print(m.get("TranscodingUrl"))' <"$work/pbi.json"
url=$(js 'print(d["MediaSources"][0]["TranscodingUrl"])' <"$work/pbi.json")
item=$(echo "$url" | cut -d/ -f3)
main=$(curl -sf "$J$url" | grep -v '^#' | head -1)
t_open=$(date +%s)
seen=""
# Play until 30 s past the end of the first break (+152 s on fakehdhr's clock).
until (($(date +%s) - t_fake > 185)); do
	for seg in $(curl -sf "$J/videos/$item/$main" | grep -v '^#'); do
		[[ " $seen " == *" $seg "* ]] && continue
		seen="$seen $seg"
		curl -sf -o "$work/s.ts" "$J/videos/$item/$seg" || { echo "segment $seg failed"; continue; }
		printf '%s +%ss %s\n' "${seg##*/}" $(($(date +%s) - t_fake)) \
			"$(ffprobe -v error -show_entries stream=codec_name,width,height,channels:format=start_time -of csv=p=0 "$work/s.ts" | tr '\n' ' ')" >>"$work/segs.txt"
	done
	sleep 2
done
# Print the first segment, every shape change, and the last segment.
awk '{shape=$3" "$4" "$5} NR==1 || shape!=prev {print} {prev=shape; last=$0} END {print last; print NR " segments"}' "$work/segs.txt"
echo "-- transcode log (errors and warnings after the open)"
docker exec "$name" sh -c 'cat /config/log/FFmpeg.LiveTvTranscode-*.log /config/log/FFmpeg.Transcode-*.log 2>/dev/null' |
	grep -iE 'discontinu|error|invalid|non[- ]monotonic|changed|reinit' | sed -E 's/previous: [0-9]+, current: [0-9]+; changing to [0-9]+/previous: N, current: M/' | sort | uniq -c | sort -rn | head -12 || true
docker exec "$name" sh -c 'grep -h -m1 -o "ffmpeg .*" /config/log/FFmpeg.*Transcode-*.log' | cut -c1-500 || true

echo "== 800.4 TOKEN: tune after the 120 s token from the guide refresh has expired"
wait_s=$((t_m3u + 125 - $(date +%s)))
((wait_s > 0)) && sleep "$wait_s"
open_live "$(chan FAST-TOKEN)" >"$work/pbi4.json"
js 'print("sources",len(d.get("MediaSources",[])),"ErrorCode",d.get("ErrorCode"))' <"$work/pbi4.json"
curl -sf http://127.0.0.1:15014/stats.json; echo
docker logs "$name" 2>&1 | grep -iE '403|Forbidden|800\.4|Error probing' | tail -3 | cut -c1-250 || true
