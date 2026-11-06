#!/usr/bin/env bash
# Credential-exposure probe: point the pinned stock Jellyfin 12.0.0 image at
# fakehdhr's synthetic Xtream panel (get.php as an M3U tuner, xmltv.php as an
# XMLTV provider; the stream URLs carry the password in their path), play a
# channel as a non-admin user, and report every API response, log and file
# that contains the synthetic password. Spike evidence for
# docs/spikes/livetv.md §5.3; not a gate. The password is a fake marker.
#
# Usage: tools/fakehdhr/xtream-probe.sh   (needs docker, curl, python3)
#        OUTPUT=m3u8 TUNERS=0 tools/fakehdhr/xtream-probe.sh
# OUTPUT=m3u8 gives .m3u8 stream URLs, which upstream never shares, so the
# raw URL becomes the media source path; TUNERS=0 also enables direct play.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
image=${TARGET:-$(awk '/image: jellyfin\/jellyfin@/ {print $2; exit}' "$root/tools/goldenrec/compose.yaml")}
name=fakehdhr-xtream-probe
J=http://127.0.0.1:18099
H='Content-Type: application/json'
P=FAKEPASS-7b1e
X="http://host.docker.internal:15016"
output=${OUTPUT:-ts}
tuners=${TUNERS:-4}

cleanup() {
	docker rm -f "$name" >/dev/null 2>&1 || true
	[[ -n ${fake_pid:-} ]] && kill "$fake_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

go build -o "$work/fakehdhr" "$root/tools/fakehdhr"
"$work/fakehdhr" -listen 0.0.0.0:15016 -advertise "$X" -discovery "" -no-fault-channels -no-fast-channels \
	-xtream-pass "$P" -xtream-movies 3 -xtream-episodes 2 -tuners 4 -overlay=false >"$work/fake.log" 2>&1 &
fake_pid=$!

docker run -d --name "$name" -p 18099:8096 --add-host host.docker.internal:host-gateway "$image" >/dev/null
for _ in $(seq 90); do
	curl -sf -X POST "$J/Startup/Configuration" -H "$H" \
		-d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}' 2>/dev/null && break
	sleep 1
done
# Throwaway credentials for the throwaway container.
curl -sf "$J/Startup/User" >/dev/null
curl -sf -X POST "$J/Startup/User" -H "$H" -d '{"Name":"probe","Password":"probe"}'
curl -sf -X POST "$J/Startup/Complete"
js() { python3 -c "import sys,json;d=json.load(sys.stdin);$1"; }
login() {
	curl -sf -X POST "$J/Users/AuthenticateByName" -H "$H" \
		-H "Authorization: MediaBrowser Client=\"probe\", Device=\"curl\", DeviceId=\"$1-dev\", Version=\"1\"" \
		-d "{\"Username\":\"$1\",\"Pw\":\"$2\"}" | js 'print(d["AccessToken"], d["User"]["Id"])'
}
read -r atok _ < <(login probe probe)
A="Authorization: MediaBrowser Token=\"$atok\""
curl -sf -X POST "$J/Users/New" -H "$A" -H "$H" -d '{"Name":"viewer","Password":"viewer"}' >/dev/null
read -r vtok vid < <(login viewer viewer)
V="Authorization: MediaBrowser Token=\"$vtok\""

curl -sf -X POST "$J/LiveTv/TunerHosts" -H "$A" -H "$H" \
	-d "{\"Type\":\"m3u\",\"Url\":\"$X/get.php?username=fakeuser&password=$P&type=m3u_plus&output=$output\",\"TunerCount\":$tuners}" >"$work/api-admin-TunerHosts-POST.json"
curl -sf -X POST "$J/LiveTv/ListingProviders" -H "$A" -H "$H" \
	-d "{\"Type\":\"xmltv\",\"Path\":\"$X/xmltv.php?username=fakeuser&password=$P\",\"EnableAllTuners\":true}" >"$work/api-admin-ListingProviders-POST.json"
task=$(curl -sf "$J/ScheduledTasks" -H "$A" | js 'print([t["Id"] for t in d if t["Key"]=="RefreshGuide"][0])')
for _ in $(seq 30); do [[ $(curl -sf "$J/ScheduledTasks/$task" -H "$A" | js 'print(d["State"])') == Running ]] && break; sleep 1; done
while [[ $(curl -sf "$J/ScheduledTasks/$task" -H "$A" | js 'print(d["State"])') != Idle ]]; do sleep 1; done

curl -sf "$J/System/Configuration/livetv" -H "$A" >"$work/api-admin-Configuration-livetv.json"
curl -sf "$J/LiveTv/ListingProviders/Default" -H "$A" >"$work/api-admin-ListingProviders-Default.json"
curl -sf "$J/LiveTv/Channels?userId=$vid" -H "$V" >"$work/api-viewer-Channels.json"
echo "channels: $(js 'print(d["TotalRecordCount"], sorted({c["Name"].split()[0] for c in d["Items"]})[:6])' <"$work/api-viewer-Channels.json")"
cid=$(js 'print([c["Id"] for c in d["Items"] if c["Name"]=="FAKE-H264"][0])' <"$work/api-viewer-Channels.json")
curl -sf "$J/Items/$cid?userId=$vid&fields=Path,MediaSources" -H "$V" >"$work/api-viewer-Items-channel.json" || true
curl -sf "$J/Users/$vid/Items/$cid" -H "$V" >"$work/api-viewer-UserItems-channel.json" || true
curl -sf "$J/LiveTv/Programs?userId=$vid&limit=20&fields=Path" -H "$V" >"$work/api-viewer-Programs.json" || true
profile='{"AutoOpenLiveStream":true,"IsPlayback":true,"DeviceProfile":{"TranscodingProfiles":[{"Container":"ts","Type":"Video","VideoCodec":"h264","AudioCodec":"aac","Protocol":"hls"}]}}'
curl -sf -X POST "$J/Items/$cid/PlaybackInfo?userId=$vid" -H "$V" -H "$H" -d "$profile" >"$work/api-viewer-PlaybackInfo.json"
url=$(js 'print(d["MediaSources"][0].get("TranscodingUrl") or "")' <"$work/api-viewer-PlaybackInfo.json")
[[ -n $url ]] && curl -sf "$J$url" -H "$V" >"$work/api-viewer-master.m3u8" || true
sleep 8
curl -sf "$J/Sessions" -H "$A" >"$work/api-admin-Sessions.json" || true
docker logs "$name" >"$work/log-container-stdout.txt" 2>&1
docker exec "$name" sh -c 'cat /config/log/*.log' >"$work/log-config-dir.txt" 2>/dev/null || true

echo "== surfaces that contain the synthetic password (count of occurrences)"
for f in "$work"/api-*.json "$work"/api-*.m3u8 "$work"/log-*.txt; do
	[[ -f $f ]] || continue
	printf '%-40s %s\n' "$(basename "$f")" "$(grep -o "$P" "$f" | wc -l | tr -d ' ')"
done
echo "== files on disk in the container that contain it"
docker exec "$name" sh -c "grep -rl '$P' /config /cache 2>/dev/null" | sed 's#/[0-9a-f]\{32\}#/<id>#g' | sort | uniq -c || true
echo "== log lines that contain it (one example per kind, password masked)"
grep -h "$P" "$work"/log-*.txt | sed -E "s/$P/<PASS>/g; s/^\[[0-9:. ]+\] //; s/[0-9a-f]{32}/<id>/g" | cut -c1-220 | sort -u | head -20
echo "== viewer PlaybackInfo media source"
js 'm=d["MediaSources"][0];print({k:(m.get(k) or "")[:90] for k in ("Path","Protocol","TranscodingUrl")}, "SupportsDirectPlay", m.get("SupportsDirectPlay"))' <"$work/api-viewer-PlaybackInfo.json" | sed "s/$P/<PASS>/g"
echo "== User-Agent per request kind, as fakehdhr saw them"
curl -sf http://127.0.0.1:15016/stats.json | python3 -c 'import sys,json;[print(f"{n:5d}  {k}") for k,n in sorted(json.load(sys.stdin)["UserAgents"].items())]'
