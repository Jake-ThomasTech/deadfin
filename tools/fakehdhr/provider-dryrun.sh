#!/usr/bin/env bash
# Dry run of the manual provider pass: the exact admin steps of the one
# read-only pass against a real Xtream panel, done here against fakehdhr's
# synthetic panel. It reports every provider-side request, so the request
# count of the real pass is known before it runs. Steps:
#
#  1. Add the get.php m3u_plus URL as an M3U tuner, with SkipVodEntries.
#  2. Add the xmltv.php URL as an XMLTV listings provider.
#  3. Wait for the Refresh Guide task that the save queues.
#  4. Read the channel list and a 6-hour guide grid (server-local reads).
#  5. Run Refresh Guide once more, to show the cached playlist and guide.
#
# No channel is opened. Live playback is not in phase 1.
#
# Usage: TARGET=deadfin:local tools/fakehdhr/provider-dryrun.sh
#        (needs docker, curl, python3)
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
image=${TARGET:-deadfin:local}
name=fakehdhr-provider-dryrun
J=http://127.0.0.1:18100
F=http://127.0.0.1:15017
H='Content-Type: application/json'

cleanup() {
	docker rm -f "$name" >/dev/null 2>&1 || true
	[[ -n ${fake_pid:-} ]] && kill "$fake_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

go build -o "$work/fakehdhr" "$root/tools/fakehdhr"
"$work/fakehdhr" -listen 0.0.0.0:15017 -advertise http://host.docker.internal:15017 -discovery "" \
	-channels 500 -no-fault-channels -no-fast-channels -guide-days 2 -tuners 0 \
	-xtream-movies 2000 -xtream-episodes 1000 -guide-share 0.35 -block-default-ua >"$work/fake.log" 2>&1 &
fake_pid=$!

docker run -d --name "$name" -p 18100:8096 --add-host host.docker.internal:host-gateway "$image" >/dev/null
for _ in $(seq 90); do
	curl -sf -X POST "$J/Startup/Configuration" -H "$H" \
		-d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}' 2>/dev/null && break
	sleep 1
done
# Throwaway credentials for the throwaway container.
curl -sf "$J/Startup/User" >/dev/null
curl -sf -X POST "$J/Startup/User" -H "$H" -d '{"Name":"dryrun","Password":"dryrun"}'
curl -sf -X POST "$J/Startup/Complete"
tok=$(curl -sf -X POST "$J/Users/AuthenticateByName" -H "$H" \
	-H 'Authorization: MediaBrowser Client="dryrun", Device="curl", DeviceId="dryrun1", Version="1"' \
	-d '{"Username":"dryrun","Pw":"dryrun"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["AccessToken"])')
A="Authorization: MediaBrowser Token=\"$tok\""
js() { python3 -c "import sys,json;d=json.load(sys.stdin);$1"; }
requests() { curl -sf "$F/stats.json" | js 'print(d["XtreamRequests"])'; }
wait_idle() {
	for _ in $(seq 120); do [[ $(curl -sf "$J/ScheduledTasks/$task" -H "$A" | js 'print(d["State"])') == Running ]] && break; sleep 1; done
	while [[ $(curl -sf "$J/ScheduledTasks/$task" -H "$A" | js 'print(d["State"])') != Idle ]]; do sleep 1; done
}

# The synthetic panel's default fake credentials.
X=http://host.docker.internal:15017
m3u="$X/get.php?username=fakeuser&password=FAKEPASS-7b1e&type=m3u_plus&output=ts"
xmltv="$X/xmltv.php?username=fakeuser&password=FAKEPASS-7b1e"
task=$(curl -sf "$J/ScheduledTasks" -H "$A" | js 'print([t["Id"] for t in d if t["Key"]=="RefreshGuide"][0])')

echo "before:            $(requests) provider requests"
curl -sf -X POST "$J/LiveTv/TunerHosts" -H "$A" -H "$H" \
	-d "{\"Type\":\"m3u\",\"Url\":\"$m3u\",\"SkipVodEntries\":true,\"TunerCount\":1}" >/dev/null
echo "1 tuner added:     $(requests)"
curl -sf -X POST "$J/LiveTv/ListingProviders" -H "$A" -H "$H" \
	-d "{\"Type\":\"xmltv\",\"Path\":\"$xmltv\",\"EnableAllTuners\":true}" >/dev/null
echo "2 provider added:  $(requests)"
wait_idle
echo "3 refresh done:    $(requests)"
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
later=$(date -u -v+6H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+6 hours' +%Y-%m-%dT%H:%M:%SZ)
channels=$(curl -sf "$J/LiveTv/Channels?limit=100" -H "$A")
ids=$(js 'print(json.dumps([c["Id"] for c in d["Items"]]))' <<<"$channels")
programs=$(curl -sf -X POST "$J/LiveTv/Programs" -H "$A" -H "$H" \
	-d "{\"ChannelIds\":$ids,\"MinEndDate\":\"$now\",\"MaxStartDate\":\"$later\"}" | js 'print(d["TotalRecordCount"])')
echo "4 reads done:      $(requests)  ($(js 'print(d["TotalRecordCount"])' <<<"$channels") channels, $programs programmes in 6 h on 100)"
curl -sf -X POST "$J/ScheduledTasks/Running/$task" -H "$A"
wait_idle
echo "5 second refresh:  $(requests)"
curl -sf "$F/stats.json" | js 'print("AuthRejects", d["AuthRejects"], "BanRejects", d["BanRejects"], "JSONEPGCalls", d["JSONEPGCalls"]); [print(f"{n:5d}  {k}") for k,n in sorted(d["UserAgents"].items())]'
