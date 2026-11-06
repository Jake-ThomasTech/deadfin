#!/usr/bin/env bash
# Add a developer's private M3U/HDHomeRun tuners and XMLTV guides to a running
# server (stock Jellyfin or Deadfin; both take the same LiveTv API). The
# source list lives outside version control, so private URLs and credentials
# never reach a commit. See docs/spikes/livetv.md §5.7.
#
# Usage: DEADFIN_LIVETV_DEV_SOURCES=~/livetv-sources.json \
#          tools/fakehdhr/dev-sources.sh http://127.0.0.1:8096 <access-token>
#
# Default file: .livetv-dev/sources.json at the repository root (gitignored).
# Format:
#   {
#     "tuners":   [{"type": "m3u", "url": "...", "userAgent": "...", "tunerCount": 2},
#                  {"type": "hdhomerun", "url": "http://192.168.1.50"}],
#     "listings": [{"type": "xmltv", "path": "https://... or /local/file.xml"}]
#   }
set -euo pipefail

server=${1:?server base URL}
token=${2:?access token}
root=$(cd "$(dirname "$0")/../.." && pwd)
file=${DEADFIN_LIVETV_DEV_SOURCES:-$root/.livetv-dev/sources.json}

[[ -f $file ]] || { echo "no sources file at $file" >&2; exit 1; }

# Refuse a sources file that git would track: it must be outside the
# repository or ignored by it.
abs=$(cd "$(dirname "$file")" && pwd)/$(basename "$file")
if [[ $abs == "$root"/* ]] && ! git -C "$root" check-ignore -q "$abs"; then
	echo "refusing $file: it is inside the repository and not gitignored" >&2
	exit 1
fi

python3 - "$file" "$server" "$token" <<'PY'
import json, sys, urllib.request

path, server, token = sys.argv[1:4]
cfg = json.load(open(path))
headers = {"Authorization": f'MediaBrowser Token="{token}"', "Content-Type": "application/json"}

def post(route, body):
    req = urllib.request.Request(server.rstrip("/") + route, json.dumps(body).encode(), headers, method="POST")
    with urllib.request.urlopen(req) as resp:
        return json.load(resp)

for t in cfg.get("tuners", []):
    body = {"Type": t["type"], "Url": t["url"]}
    if "userAgent" in t:
        body["UserAgent"] = t["userAgent"]
    if "tunerCount" in t:
        body["TunerCount"] = t["tunerCount"]
    got = post("/LiveTv/TunerHosts", body)
    # Print the type and id only; the URL may carry credentials.
    print("tuner", got.get("Type"), got.get("Id"))

for l in cfg.get("listings", []):
    got = post("/LiveTv/ListingProviders?validateListings=true",
               {"Type": l.get("type", "xmltv"), "Path": l["path"], "EnableAllTuners": True})
    print("listings", got.get("Type"), got.get("Id"))
PY
