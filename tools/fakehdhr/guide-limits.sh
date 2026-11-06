#!/usr/bin/env bash
# Guide-import limits run against the pinned stock Jellyfin image: generate a
# lineup of CHANNELS channels with DAYS of XMLTV and the given QUIRKS, add it
# as an M3U tuner plus XMLTV provider, and report guide import time, peak
# container memory, database size, and guide-grid query latency as one JSON
# line, with the slowest user-data write during the refresh. The same
# measurements are the acceptance metrics for Deadfin
# (docs/spikes/livetv.md §5.2); point TARGET at a Deadfin image to compare,
# for example TARGET=deadfin:local after docker build -t deadfin:local .
# GUIDE_ANCHOR sets the fakehdhr guide start (default: the current UTC hour),
# so two runs can share one guide.
#
# Usage: CHANNELS=1000 DAYS=7 QUIRKS=all tools/fakehdhr/guide-limits.sh
#
# XTREAM=1 imports through the synthetic Xtream panel instead (get.php
# m3u_plus with MOVIES and EPISODES VOD entries, xmltv.php covering
# GUIDE_SHARE of the live channels). TIMEBOX=s stops waiting for the refresh
# after s seconds and reports the progress rate instead, for runs that
# would take hours on stock. KEEP=1 leaves the container running for
# inspection (docker rm -f fakehdhr-guide-limits). SKIP_VOD=1 sets SkipVodEntries on the tuner
# (a Deadfin-only field, D-78).
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
channels=${CHANNELS:-1000}
days=${DAYS:-7}
quirks=${QUIRKS:-none}
xtream=${XTREAM:-0}
movies=${MOVIES:-0}
episodes=${EPISODES:-0}
guide_share=${GUIDE_SHARE:-1}
timebox=${TIMEBOX:-0}
skip_vod=$([[ ${SKIP_VOD:-0} == 1 ]] && echo true || echo false)
image=${TARGET:-$(awk '/image: jellyfin\/jellyfin@/ {print $2; exit}' "$root/tools/goldenrec/compose.yaml")}
work=$(mktemp -d)
name=fakehdhr-guide-limits
J=http://127.0.0.1:18098
H='Content-Type: application/json'

cleanup() {
	[[ ${KEEP:-0} == 1 ]] || docker rm -f "$name" >/dev/null 2>&1 || true
	[[ -n ${fake_pid:-} ]] && kill "$fake_pid" 2>/dev/null || true
	[[ -n ${mem_pid:-} ]] && kill "$mem_pid" 2>/dev/null || true
	[[ -n ${write_pid:-} ]] && kill "$write_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

go build -o "$work/fakehdhr" "$root/tools/fakehdhr"
"$work/fakehdhr" -listen 0.0.0.0:15012 -advertise http://host.docker.internal:15012 -discovery "" \
	-channels "$channels" -no-fault-channels -no-fast-channels -guide-days "$days" -quirks "$quirks" \
	-guide-anchor "${GUIDE_ANCHOR:-$(date -u +%Y-%m-%dT%H:00:00Z)}" -tuners 0 \
	-xtream-movies "$movies" -xtream-episodes "$episodes" -guide-share "$guide_share" >"$work/fake.log" 2>&1 &
fake_pid=$!

docker rm -f "$name" >/dev/null 2>&1 || true
# TARGET_ENV passes NAME=value settings to the server container.
env_args=()
for setting in ${TARGET_ENV:-}; do env_args+=(-e "$setting"); done
docker run -d --name "$name" -p 18098:8096 --add-host host.docker.internal:host-gateway "${env_args[@]}" "$image" >/dev/null
for _ in $(seq 90); do
	curl -sf -X POST "$J/Startup/Configuration" -H "$H" \
		-d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}' 2>/dev/null && break
	sleep 1
done
# Throwaway credentials for the throwaway container.
curl -sf "$J/Startup/User" >/dev/null
curl -sf -X POST "$J/Startup/User" -H "$H" -d '{"Name":"limits","Password":"limits"}'
curl -sf -X POST "$J/Startup/Complete"
auth=$(curl -sf -X POST "$J/Users/AuthenticateByName" -H "$H" \
	-H 'Authorization: MediaBrowser Client="limits", Device="curl", DeviceId="limits1", Version="1"' \
	-d '{"Username":"limits","Pw":"limits"}')
js() { python3 -c "import sys,json;d=json.load(sys.stdin);$1"; }
tok=$(js 'print(d["AccessToken"])' <<<"$auth")
uid=$(js 'print(d["User"]["Id"])' <<<"$auth")
A="Authorization: MediaBrowser Token=\"$tok\""

# Sample container memory once a second for the peak.
(while sleep 1; do docker stats --no-stream --format '{{.MemUsage}}' "$name" 2>/dev/null | awk '{print $1}'; done >"$work/mem.log") &
mem_pid=$!

curl -sf -X POST "$J/System/Configuration/livetv" -H "$A" -H "$H" -d "{\"GuideDays\":$days}" >/dev/null || true
t0=$(python3 -c 'import time;print(time.time())')
F=http://host.docker.internal:15012
m3u_url=$F/playlist.m3u xmltv_url=$F/xmltv.xml
if [[ $xtream == 1 ]]; then
	# The synthetic panel's default fake credentials.
	m3u_url="$F/get.php?username=fakeuser&password=FAKEPASS-7b1e&type=m3u_plus&output=ts"
	xmltv_url="$F/xmltv.php?username=fakeuser&password=FAKEPASS-7b1e"
fi
curl -sf -o "$work/m3u" "${m3u_url/host.docker.internal/127.0.0.1}"
curl -sf -o "$work/xmltv" "${xmltv_url/host.docker.internal/127.0.0.1}"
m3u_entries=$(grep -c '^#EXTINF' "$work/m3u")
xmltv_mb=$(($(wc -c <"$work/xmltv") / 1000000))
curl -sf -X POST "$J/LiveTv/TunerHosts" -H "$A" -H "$H" -d "{\"Type\":\"m3u\",\"Url\":\"$m3u_url\",\"TunerCount\":0,\"SkipVodEntries\":$skip_vod}" >/dev/null
curl -sf -X POST "$J/LiveTv/ListingProviders" -H "$A" -H "$H" \
	-d "{\"Type\":\"xmltv\",\"Path\":\"$xmltv_url\",\"EnableAllTuners\":true}" >/dev/null
task=$(curl -sf "$J/ScheduledTasks" -H "$A" | js 'print([t["Id"] for t in d if t["Key"]=="RefreshGuide"][0])')
view_id=$(curl -sf "$J/UserViews?userId=$uid" -H "$A" | js 'print([i["Id"] for i in d["Items"] if i.get("CollectionType")=="livetv"][0])')
# Wait for the queued refresh to start, then to finish.
for _ in $(seq 120); do [[ $(curl -sf "$J/ScheduledTasks/$task" -H "$A" | js 'print(d["State"])') == Running ]] && break; sleep 1; done
t_run=$(date +%s)
# While the refresh runs, toggle a favourite on the Live TV view twice a
# second: a user-data write that needs the database writer.
# WRITE_LOG keeps the per-write log (Unix time, status, seconds).
write_log=${WRITE_LOG:-$work/write.log}
(while sleep 0.5; do
	for method in POST DELETE; do
		echo "$(date +%s) $(curl -s -o /dev/null -w '%{http_code} %{time_total}' -X $method "$J/UserFavoriteItems/$view_id?userId=$uid" -H "$A")"
	done
done >"$write_log") &
write_pid=$!
timed_out=false
while [[ $(curl -sf "$J/ScheduledTasks/$task" -H "$A" | js 'print(d["State"])') != Idle ]]; do
	if ((timebox > 0 && $(date +%s) - t_run > timebox)); then timed_out=true; break; fi
	sleep 2
done
t1=$(python3 -c 'import time;print(time.time())')
kill "$write_pid" 2>/dev/null || true
read -r write_count write_failed write_max_ms < <(python3 -c '
import sys
rows = [line.split()[1:] for line in open(sys.argv[1]) if line.strip()]
ok = [float(t) for code, t in rows if code.startswith("2")]
print(len(rows), len(rows) - len(ok), round(max(ok or [0]) * 1000))' "$write_log")
# One "xmltv path" log line per channel whose programmes were read.
parsed=$(docker logs "$name" 2>&1 | grep -c 'xmltv path' || true)
progress=$(curl -sf "$J/ScheduledTasks/$task" -H "$A" | js 'print(round(d.get("CurrentProgressPercentage") or 0, 1))')
result=$(curl -sf "$J/ScheduledTasks/$task" -H "$A" | js 'print((d.get("LastExecutionResult") or {}).get("Status"))')

grid_ms() { curl -s -o /dev/null -w '%{time_total}' "$@" | python3 -c 'import sys;print(round(float(sys.stdin.read())*1000))'; }
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
later=$(date -u -v+6H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+6 hours' +%Y-%m-%dT%H:%M:%SZ)
ids=$(curl -sf "$J/LiveTv/Channels?limit=100" -H "$A" | js 'print(json.dumps([c["Id"] for c in d["Items"]]))')
chan_count=$(curl -sf "$J/LiveTv/Channels?limit=1" -H "$A" | js 'print(d["TotalRecordCount"])')
prog_count=$(curl -sf "$J/LiveTv/Programs?limit=1" -H "$A" | js 'print(d["TotalRecordCount"])')
channels_ms=$(grid_ms "$J/LiveTv/Channels?limit=100&addCurrentProgram=true" -H "$A")
grid=$(grid_ms -X POST "$J/LiveTv/Programs" -H "$A" -H "$H" -d "{\"ChannelIds\":$ids,\"MinEndDate\":\"$now\",\"MaxStartDate\":\"$later\",\"EnableImages\":true,\"SortBy\":[\"StartDate\"]}")
# The database file of stock (/config) or Deadfin (/var/lib/deadfin).
db=$(docker exec "$name" sh -c 'du -k /config/data/jellyfin.db /var/lib/deadfin/data/deadfin.db 2>/dev/null | head -1 | cut -f1')
peak=$(sort -h "$work/mem.log" | tail -1)
# The server process's peak RSS; docker stats also counts the page cache.
peak_rss_mb=$(docker exec "$name" sh -c 'awk "/VmHWM/ {print int(\$2/1024)}" /proc/1/status' 2>/dev/null || echo 0)

python3 - <<PY
import json
print(json.dumps({"target": "$image"[:40], "channels": $channels, "days": $days, "quirks": "$quirks",
  "xtream": $xtream, "m3u_entries": $m3u_entries, "xmltv_mb": $xmltv_mb, "timed_out": "$timed_out" == "true",
  "progress_pct": $progress, "xmltv_parses": $parsed,
  "refresh_status": "$result", "guide_import_s": round($t1 - $t0, 1), "peak_mem": "$peak", "peak_rss_mb": int("$peak_rss_mb" or 0),
  "db_kb": int("$db" or 0), "channels_seen": $chan_count, "programs_seen": $prog_count,
  "channels_100_current_ms": $channels_ms, "grid_100ch_6h_ms": $grid,
  "user_writes": $write_count, "user_writes_failed": $write_failed, "user_write_max_ms": $write_max_ms}))
PY
