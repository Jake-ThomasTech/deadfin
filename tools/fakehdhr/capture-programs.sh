#!/usr/bin/env bash
# Capture the stock guide grid for the Live TV read API acceptance test.
#
# The script starts fakehdhr at a fixed guide anchor and points the pinned
# stock Jellyfin image at its M3U playlist and XMLTV guide. It then asks
# stock for POST /LiveTv/Programs on the first 6 channels over 3 days, as
# the jellyfin-web guide does. It writes the playlist, the guide and stock's
# answer to internal/api/handlers/testdata/livetv-stock-programs.json.
# TestLiveTvProgramsMatchStock replays the playlist and guide into Deadfin
# at the capture time and compares names and start dates.
#
# Usage: tools/fakehdhr/capture-programs.sh   (needs docker, curl, python3)
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
image=$(awk '/image: jellyfin\/jellyfin@/ {print $2; exit}' "$root/tools/goldenrec/compose.yaml")
name=fakehdhr-capture-programs
J=http://127.0.0.1:18097
F=http://127.0.0.1:15005
H='Content-Type: application/json'
out="$root/internal/api/handlers/testdata/livetv-stock-programs.json"
# The anchor is the current UTC hour, so stock's guide window, which starts
# at stock's real clock, covers the guide.
anchor=$(date -u +%Y-%m-%dT%H:00:00Z)

cleanup() {
	docker rm -f "$name" >/dev/null 2>&1 || true
	[[ -n ${fake_pid:-} ]] && kill "$fake_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

go build -o "$work/fakehdhr" "$root/tools/fakehdhr"
"$work/fakehdhr" -listen 0.0.0.0:15005 -advertise http://host.docker.internal:15005 -discovery "" \
	-no-fault-channels -no-fast-channels -overlay=false -guide-days 3 -guide-anchor "$anchor" >"$work/fake.log" 2>&1 &
fake_pid=$!
for _ in $(seq 30); do curl -sf "$F/playlist.m3u" -o "$work/playlist.m3u" && break; sleep 0.2; done
curl -sf "$F/xmltv.xml" -o "$work/guide.xml"

docker run -d --name "$name" -p 18097:8096 --add-host host.docker.internal:host-gateway "$image" >/dev/null
for _ in $(seq 90); do
	curl -sf -X POST "$J/Startup/Configuration" -H "$H" \
		-d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}' 2>/dev/null && break
	sleep 1
done

# Throwaway credentials for the throwaway container.
curl -sf "$J/Startup/User" >/dev/null
curl -sf -X POST "$J/Startup/User" -H "$H" -d '{"Name":"capture","Password":"capture"}'
curl -sf -X POST "$J/Startup/Complete"
auth=$(curl -sf -X POST "$J/Users/AuthenticateByName" -H "$H" \
	-H 'Authorization: MediaBrowser Client="capture", Device="curl", DeviceId="capture1", Version="1"' \
	-d '{"Username":"capture","Pw":"capture"}')
tok=$(python3 -c 'import sys,json;print(json.load(sys.stdin)["AccessToken"])' <<<"$auth")
uid=$(python3 -c 'import sys,json;print(json.load(sys.stdin)["User"]["Id"])' <<<"$auth")
A="Authorization: MediaBrowser Token=\"$tok\""
js() { python3 -c "import sys,json;d=json.load(sys.stdin);$1"; }

curl -sf -X POST "$J/LiveTv/TunerHosts" -H "$A" -H "$H" \
	-d '{"Type":"m3u","Url":"http://host.docker.internal:15005/playlist.m3u"}' >/dev/null
curl -sf -X POST "$J/LiveTv/ListingProviders" -H "$A" -H "$H" \
	-d '{"Type":"xmltv","Path":"http://host.docker.internal:15005/xmltv.xml","EnableAllTuners":true}' >/dev/null
task=$(curl -sf "$J/ScheduledTasks" -H "$A" | js 'print([t["Id"] for t in d if t["Key"]=="RefreshGuide"][0])')
# Saving the listing provider queues a refresh; wait for it, then run one more.
for pass in 1 2; do
	for _ in $(seq 120); do
		[[ $(curl -sf "$J/ScheduledTasks/$task" -H "$A" | js 'print(d["State"])') == Idle ]] && break
		sleep 1
	done
	[[ $pass == 1 ]] && curl -sf -X POST "$J/ScheduledTasks/Running/$task" -H "$A" && sleep 2
done
captured=$(date -u +%Y-%m-%dT%H:%M:%SZ)

curl -sf "$J/LiveTv/Channels?userId=$uid&addCurrentProgram=false&enableImages=false" -H "$A" >"$work/channels.json"
ids=$(js 'print(",".join("\""+c["Id"]+"\"" for c in d["Items"][:6]))' <"$work/channels.json")
end=$(python3 -c "import datetime as t;print((t.datetime.fromisoformat('${anchor%Z}')+t.timedelta(days=3)).strftime('%Y-%m-%dT%H:%M:%SZ'))")
curl -sf -X POST "$J/LiveTv/Programs" -H "$A" -H "$H" -d "{\"UserId\":\"$uid\",\"ChannelIds\":[$ids],\"MinEndDate\":\"$anchor\",\"MaxStartDate\":\"$end\",\"SortBy\":[\"StartDate\"],\"EnableImages\":false,\"EnableUserData\":false,\"EnableTotalRecordCount\":true,\"Fields\":[\"ChannelInfo\"]}" >"$work/programs.json"

mkdir -p "$(dirname "$out")"
python3 - "$work" "$anchor" "$captured" "$end" "$image" >"$out" <<'EOF'
import json, sys
work, anchor, captured, end, image = sys.argv[1:]
channels = json.load(open(f"{work}/channels.json"))["Items"][:6]
programs = json.load(open(f"{work}/programs.json"))
json.dump({
    "image": image,
    "anchor": anchor,
    "captured": captured,
    "minEndDate": anchor,
    "maxStartDate": end,
    "playlist": open(f"{work}/playlist.m3u").read(),
    "guide": open(f"{work}/guide.xml").read(),
    "channels": [{"Number": c.get("ChannelNumber", ""), "Name": c["Name"]} for c in channels],
    "totalRecordCount": programs["TotalRecordCount"],
    "programs": [{"ChannelNumber": p.get("ChannelNumber", ""), "Name": p["Name"], "StartDate": p["StartDate"]} for p in programs["Items"]],
}, sys.stdout, indent=1)
print()
EOF
echo "wrote $out: $(js 'print(len(d["programs"]))' <"$out") programmes on $(js 'print(len(d["channels"]))' <"$out") channels"
