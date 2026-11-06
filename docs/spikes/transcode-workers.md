# Spike: distributed transcoding (remote GPU workers)

Status: design record. The spike (#538, 2026-10-13) is superseded by the
v0.4.0 phase issues on `train/transcode-workers`: #614 (worker runtime on
the main listener), #615 (identities and admin API), #616 (placement),
#617 (drain and signals), #622 (hardware placement), #623 (packaging),
#624 (rest of the transcode surface), and the redeploy-continuity issues
#618–#621 (operator requirement of 2026-10-16: a server redeploy does not
interrupt playback). The issues carry the current spec; where they differ
from this document, the issue wins. #614 replaced the experimental listener
below with routes on the main listener. #615 added join tokens and worker
identities in the database, managed through `/deadfin/v1/transcode-workers`,
beside static tokens in `DEADFIN_TRANSCODE_WORKER_TOKENS` for GitOps setups
(D21). #616 added the placement policy below, stored in the database and
managed through `/deadfin/v1/transcode-workers/policy`;
`DEADFIN_TRANSCODE_WORKER_MODE` sets its mode at startup. In the built
schema a rule carries `pools` or `local` and its overrides directly (no
`action` object), the bit-depth bounds are `bitDepthMin` and
`bitDepthMax`, and `queueTimeout` is at most 60 s. #617 added drain and
handoff, quarantine, the ffmpeg version check, and the §3.9 signals; the
metrics are hand-written Prometheus text (no client library), and the
scaling token comes from `DEADFIN_TRANSCODE_SCALING_TOKENS` or from
`/deadfin/v1/transcode-workers/scaling-token`. #618 added job leases: a
worker keeps its jobs through a server restart, and the next server
process adopts them (§3.7). The lease is `DEADFIN_TRANSCODE_JOB_LEASE`
(60 s by default, `0` turns it off). The divergence rows are D-06 and D-118.

Question: can Deadfin run its live HLS transcodes on other hosts (GPU boxes,
optionally Kubernetes nodes), so it is not tied to one machine? What would the
design be, what does it cost, and how does it fail?

Short answer:

1. **Yes, and the seam is small.** The server keeps the playback decision,
   the HLS contract, the segment readiness rule and the seek/restart policy.
   Only the ffmpeg process moves. The seam is one interface in
   `internal/ffmpeg` (`RemoteRunner`), which the `TranscodeManager` offers an
   HLS family to before it starts ffmpeg locally. `internal/encoding` stays
   pure and the client URLs and playlists do not change. The prototype proves
   it: the playlist hash is the same in all three modes (§5).
2. **The community shape holds, with three corrections.** An agent per box
   that dials out, registers with a join token, heartbeats and takes jobs,
   with no scheduler, is right. The corrections:
   - Hardware placement must happen *before* the argv is built. The argv
     names the encoder, the device and the filters, so phase 2 moves
     placement above the seam and computes the argv from the worker's
     capabilities (§3.2).
   - The worker *pushes* segments; the server never pulls. This keeps NAT
     working and keeps one writer for the transcode directory (§3.4).
   - A heartbeat alone fails over too slowly for a waiting client. Phase 1
     needs a segment-progress watchdog (§3.7).
3. **Over loopback, remote costs nothing measurable.** On this Mac (M4 Max,
   software x264, 1080p → 720p, 6 s segments) the time to first segment,
   the seek latency and the failover time are within the 100 ms poll quanta
   of local transcoding (§5). The real costs are network costs: the
   segment upload is on the critical path, and the streamed source reads
   about 3× the bytes that ffmpeg encodes because of read-ahead.
4. **Security is bounded by the job.** A worker sees only the sources of
   the jobs placed on it and can write only the file names of those jobs'
   playlist families. It never sees the API, the database or other users.
   The join token is never sent on the wire. TLS is required off loopback
   (§3.5).
5. **Placement is configurable.** A server-wide mode (`local`,
   `local-first`, `workers-first`, `workers-only`) sets the default;
   ordered per-codec rules on top send a job to a worker pool or keep it
   local ("AV1 or 10-bit HEVC decode, or tone mapping, goes to `big-gpu`");
   the first match wins. A job with no free worker can queue for a worker
   that autoscale is starting. Every path ends in one fallback chain: a
   worker, then the local GPU, then the local CPU (on by default, can be
   turned off), then an error (§3.8).
6. **Deadfin scales nothing itself.** It exports per-pool scaling signals
   (Prometheus and JSON) and an optional scale hook; KEDA or a script
   scales the workers, with a warm floor for the common case. Workers drain
   on SIGTERM and hand their jobs off in about a second (§3.9).
7. **Upstream has nothing built in.** This is a native extension beyond
   parity. It needs a divergence row when it leaves the experimental flag
   (§7).

The prototype is behind `DEADFIN_EXPERIMENTAL_TRANSCODE_WORKERS_LISTEN`. It
is off unless that variable is set.

---

## 1. Upstream and prior art

### 1.1 Jellyfin

Pinned Jellyfin (`b04614e`) runs every ffmpeg as a local child process
(`TranscodeManager.StartFfMpeg`). There is no remote transcoding, no worker
concept and no plugin seam for it. The community answer is rffmpeg.

### 1.2 rffmpeg

rffmpeg is a wrapper that Jellyfin calls in place of `ffmpeg`. It picks a
host from a local SQLite state file (by weight and by the count of running
processes), runs the same argv there over SSH, and falls back to localhost
when no host answers. Requirements and limits:

- The remote host must see **both** the media and Jellyfin's transcode
  directory at the **same paths** as the server, normally over NFS. ffmpeg
  writes the segments straight into the shared transcode directory.
- The server holds an SSH private key for every remote host.
- Docker is not officially supported, because of the path parity and NFS
  export complexity.
- A host that dies mid-stream is not handled. The client sees a stall
  until Jellyfin's own restart logic happens to relaunch ffmpeg, and that
  relaunch goes through rffmpeg again.

### 1.3 Tdarr

Tdarr is a batch library transcoder, not a live one, but its node model is
what the community asked for:

- A **server** owns the library and the queue. **Nodes** connect to the
  server, report their worker limits per job type (CPU or GPU, transcode or
  health check), and take jobs.
- **Mapped nodes** see the library through shared mounts and **path
  translators** (server path → node path).
- **Unmapped nodes** (beta, 2.27.01, Tdarr Pro) need no shared storage:
  the server sends the working file to the node and takes the result back.
  They support only plugins that act on the working file; side files stay
  on the node.

### 1.4 What carries over

| Idea | Source | In Deadfin |
|---|---|---|
| Agent per box that dials out and takes work | Tdarr nodes | Yes: `deadfin worker` dials out over one WebSocket |
| Per-node job limits by kind | Tdarr worker limits | Yes: `MaxJobs`, set to the hardware session limit (#420) |
| Path translators for shared storage | Tdarr mapped nodes | Yes: `--path-map /server=/local`; the worker uses it only when the mapped file exists with the expected size |
| Server moves the file when there is no shared storage | Tdarr unmapped nodes | Yes, but streamed by range on demand, not copied up front: a live job must start in about a second |
| Same argv, run elsewhere | rffmpeg | Yes for phase 1. Phase 2 builds the argv for the worker's capabilities instead |
| Fall back to localhost | rffmpeg | Yes: any placement failure runs the job locally |
| Shared transcode directory written by remote ffmpeg | rffmpeg | **No.** The server stays the only writer of its transcode directory. The worker uploads, and the server lands each file by rename |
| SSH keys on the server | rffmpeg | **No.** The worker dials out; the server holds no credential for the worker |

## 2. The community shape, validated

| Proposal | Verdict | Notes |
|---|---|---|
| Same binary, `deadfin worker` mode | Keep | Built. The container image already has jellyfin-ffmpeg |
| Container, rpm/deb, optional Helm DaemonSet | Keep | Phase 3. Kubernetes is one option; a systemd unit is the other |
| Register with a join token from the admin panel | Keep, with mutual proof | Decided: one join token mints a revocable per-worker identity at first registration (§3.3). The prototype reads tokens from the environment |
| Report capabilities and load | Keep | The prototype reports hwaccel families, the probed encoder list, the ffmpeg version and `MaxJobs`, and heartbeats the active job count |
| Dial out over a long-lived connection | Keep | One WebSocket for control. Data uses plain HTTP requests on the same origin |
| Mutual auth via the token, TLS off loopback | Keep | HMAC challenge in both directions; the secret never crosses the wire |
| Seam at `internal/ffmpeg` | Keep for phase 1 | Correct for "run this argv elsewhere". Not enough for hardware placement (§3.2) |
| Shared path or server-streamed input | Keep both | Decided: a per-worker setting, `shared`, `stream` or `auto` (§3.3). The prototype is `auto` |
| Worker pushes segments, or server pulls | **Push only** | Pull needs an inbound port on the worker and a second writer protocol |
| Restart on another worker or locally on failure | Keep | Uses the existing restart path. Needs a progress watchdog for partitions (§3.7) |
| No scheduler; kill nodes any time | Keep | Placement is a pure function of the rules and the registered workers at start time (§3.8). Nothing is rebalanced |

## 3. Design

### 3.1 Seam location

```
 client ──HTTP──▶ handlers ──▶ playback.HLSService ──▶ TranscodeManager ──┬──▶ local ffmpeg (exec, pgid)
                                  (decision, argv)      (readiness,       │
                                                         restart, idle,   └──▶ RemoteRunner ──▶ Hub ══WS══ worker ──▶ ffmpeg
                                                         throttle, cleanup)        (placement, data plane)
```

`ffmpeg.RemoteRunner` (`internal/ffmpeg/runner.go`):

```go
type RemoteRunner interface {
    StartHLS(ctx context.Context, job RemoteHLSJob) (RemoteProcess, error)
}
type RemoteProcess interface {
    Stdin() io.Writer // "p", "u", "q\n": the throttle and stop protocol
    Wait() error      // ErrRunnerLost when the host vanished
    Kill()
}
```

Inside the manager, the old `*exec.Cmd` field is now a small `process`
interface (`wait`, `kill`, `reap`) with a local and a remote implementation.
Everything else is unchanged: readiness polling (segment N is ready when
N+1 exists or the job exited), `-start_number` restarts, the 24/segment
restart threshold, the throttler (its keystrokes go to `Stdin`), the
segment cleaner, the idle kill, and the stderr progress parser (the worker
forwards stderr). `startRemote` offers only HLS families with no subtitle
burn-in; anything else, or any refusal, starts locally.

`internal/encoding` is untouched. The HLS service gained one behavior: when
`WaitSegment` reports `ErrHLSRunnerLost`, it calls `RestartHLSAt` for the
same segment once and waits again.

### 3.2 What the seam cannot do: hardware placement

The argv already names the encoder (`h264_qsv`), the device
(`-init_hw_device vaapi=va:/dev/dri/renderD128`) and the filter chain.
These come from the **server's** `encoding.Capabilities`. A runner at the
ffmpeg seam can only check that a worker can run that exact argv. The
prototype does this: it extracts the `-hwaccel`, `-init_hw_device` and
`-c:v` requirements and places the job only on a worker that reports them.
The worker must also expose the same device node path.

That is enough for a homogeneous fleet ("every box has an Intel iGPU"). It
is wrong for a mixed fleet: a server with no GPU would always build a
libx264 argv and never use an NVENC worker.

Phase 2 fix: **place first, then build the argv.** `encoding` is already a
pure function of `(EncodingJob, Capabilities)`. The playback service asks
the runner for a placement, receives the worker's `encoding.Capabilities`
(probed on the worker at registration), builds the argv with them, and
starts the job on that placement. A placement that is lost later restarts
through the same path, so the replacement argv fits the replacement host.

Built in #622:

- **Worker report.** `deadfin worker -hw-type qsv|vaapi|nvenc
  -render-node /dev/dri/renderD128 -hw-sessions N -hw-decode CODECS`
  runs the server's probes on the worker (ffmpeg capabilities, the NVENC
  encoders that open, the VAAPI driver) and reports the full
  `encoding.Capabilities` and its hardware (`encoding.HostHardware`) at
  registration. Without `-hwaccel`, the device families default to those
  of `-hw-type` that the worker's ffmpeg supports.
- **Argv per worker.** The playback service gives the manager an argv
  builder with each video transcode (`HLSRequest.BuildArgs`). For each
  worker profile the hub builds the argv with the worker's capabilities
  and `HardwareOptions.ForHost` (the worker's family, render nodes and
  decoding codecs; the server's tone mapping, low power, codec and preset
  options), then checks it against the worker as before. A worker whose
  argv does not build (for example tone mapping without `tonemapx`) does
  not fit. A worker that reports no encoding capabilities gets the
  server's argv. The output operands, the family name and the rendition
  identity stay the server's.
- **Preference and limits.** Among the workers that fit, placement takes
  a worker whose argv uses its hardware before a software one, then
  applies the strategy. `-hw-sessions` bounds the jobs whose argv uses
  the hardware family (NVENC sessions per GPU, #420); a worker at the
  limit does not fit, and the chain goes on to the next worker, the local
  GPU, or local software.
- **Device nodes.** The argv names the worker's render node, so a worker
  may expose its GPU at another path than the server's.

### 3.3 Protocol

Decided: the worker endpoints are on the **main listener** under
`/deadfin/v1/workers/`, so they use the existing port, ingress and TLS. The
prototype still uses a separate env-gated listener with the paths below
without the prefix.

| Path | Use | Authorization |
|---|---|---|
| `GET /deadfin/v1/workers/connect` | Control WebSocket | Worker identity proof in the first messages (below); no user session |
| `GET /deadfin/v1/workers/jobs/{id}/source` | Streamed source | Per-job bearer token |
| `PUT /deadfin/v1/workers/jobs/{id}/files/{name}` | Artifact upload | Per-job bearer token |

The routes register with a `worker` policy on route registration, not the
user policies. User tokens and API keys are refused on these paths, and a
worker credential is refused on every other path. The ingress must pass the
WebSocket upgrade, keep an idle timeout above the heartbeat interval, and
allow request bodies up to the artifact limit (512 MiB).

#### Join token and worker identity

Decided: one **join token** mints a **per-worker identity** at first
registration. The token also carries the server certificate fingerprint.

```
dfw1.<joinId>.<secret>.<fingerprint>
```

- `joinId`, `secret`: as in the prototype. The server stores
  `HMAC(secret, label)`, never the secret.
- `fingerprint`: base64url SHA-256 of the server leaf certificate's
  SubjectPublicKeyInfo, or empty (`dfw1.<id>.<secret>.`) when the server
  certificate chains to a CA the worker trusts. With a fingerprint, the
  worker accepts exactly that key and skips CA validation, so a self-signed
  certificate works. The server shows the fingerprint of the certificate
  that the client sees; behind an ingress, the operator pastes or uploads
  the ingress certificate (the server cannot see it).
- A join token has an optional expiry and an optional use count. Revoking
  it stops new enrollments only.

Enrollment (first connection, worker has no identity file):

1. The worker runs the mutual HMAC handshake below with the join key.
2. The server sends `enrolled {workerId, workerSecret}` over the
   authenticated channel. It stores the worker row (id, name, join token
   id, derived key, input mode, created, last seen, revoked).
3. The worker writes `{workerId, workerSecret, fingerprint}` to
   `<state-dir>/identity.json` (mode 0600) and uses it from then on.

Every later connection runs the same handshake with the worker key
(`tokenId` = `workerId`). Revoking a worker deletes its key and closes its
live connection; its jobs are lost and restart (§3.7). A worker whose
identity is revoked does not re-enroll by itself: the operator gives it a
new join token.

Certificate rotation: with a fingerprint, a new server key breaks the pin.
The server sends `pins {fingerprints}` over the authenticated channel when
the operator adds the next key, and the worker adds it to its identity file
before the rotation. Without a fingerprint, ordinary CA rotation applies.

The proofs are not bound to the TLS channel (behind an ingress the server
has no TLS exporter value). Certificate verification, by CA or by pin, is
what keeps a man in the middle out.

#### Input mode

Decided: a **per-worker input setting**, stored on the worker row and set
by the operator:

| Mode | Behavior |
|---|---|
| `shared` | The worker reads only a mapped path. The server sends no source URL, and the data plane refuses the source path for this worker. A job whose source is outside the worker's reported path maps is not placed on it |
| `stream` | The worker always streams the source from the server |
| `auto` (default) | A mapped path when the file exists with the expected size; the stream otherwise (the prototype) |

The worker reports its path-map server prefixes at registration, so the
server can check `shared` placements before it starts a job. `shared` is
the setting for WAN workers and for high-bitrate remuxes (§5: the stream
reads about 3× the encoded span).

#### Control channel

One WebSocket per worker, JSON messages
(`internal/transcodeworker/protocol.go`), protocol version 1.

```
worker                                   server
  │ hello {proto, tokenId, name, nonceW}    │
  │────────────────────────────────────────▶│ key = keys[tokenId] (else reject)
  │ challenge {nonceS, proofS}              │ proofS = HMAC(key, "server"‖nonceW‖nonceS)
  │◀────────────────────────────────────────│
  │ verify proofS (else disconnect)         │
  │ auth {proofW, capabilities}             │ proofW = HMAC(key, "worker"‖nonceW‖nonceS)
  │────────────────────────────────────────▶│ verify (else reject)
  │ [enrolled {workerId, workerSecret}]     │ first connection with a join token only
  │◀────────────────────────────────────────│
  │ welcome {workerId, heartbeatMillis}     │
  │◀────────────────────────────────────────│
  │ heartbeat {activeJobs}   every 5 s      │ silent for > 3 intervals ⇒ close, jobs lost
  │────────────────────────────────────────▶│
  │ job.start {id, token, args, source…}    │ placement by rules (§3.8)
  │◀────────────────────────────────────────│
  │ job.started | job.failed                │
  │────────────────────────────────────────▶│
  │ job.stderr {data}  (progress lines)     │ → manager's stderr sidecars
  │────────────────────────────────────────▶│
  │ job.stdin {data} | job.kill             │ ← throttler, stop
  │◀────────────────────────────────────────│
  │ job.exited {error}                      │ slot released
  │────────────────────────────────────────▶│
```

Data plane: plain HTTP on the same origin, authorized by a random per-job
bearer token that dies with the job.

- `GET …/jobs/{id}/source`: the pinned source, served with
  `http.ServeContent` from the manager's already-open descriptor (`ReadAt`
  only), so ranges and `-ss` seeks work. The pin is the same one a local
  job uses: a file replaced mid-job cannot be read.
- `PUT …/jobs/{id}/files/{name}` with `Deadfin-Artifact-State: growing |
  complete`: one file of the job's playlist family. `complete` lands by
  temp file and rename, so a reader never sees a partial file. `growing`
  creates an empty placeholder.

Argv rewriting (`buildJobSpec`): the source operand becomes `{{source}}` and
the playlist and segment pattern become `{{out}}/<name>`. Any other absolute
path except `/dev/*`, any `fd:` or `pipe:` operand, and any second source
**refuse the job** (it then runs locally). The demo found why this guard
matters: on macOS `/var` is a symlink to `/private/var`, the first rewrite
compared aliased paths, and the worker on the same host wrote straight into
the server's directory. The manager now canonicalizes the output operands
before it offers a job, and the hub refuses any absolute path it did not
rewrite.

### 3.4 Output: push, and the growing placeholder

The worker polls its job directory every 100 ms and applies the server's
readiness rule locally: segment N is complete when a later segment exists
or ffmpeg exited. It uploads N, then deletes its local copy (worker disk
stays bounded). If it uploaded only complete segments, the server would see
N+1 only after the worker saw N+2, and every segment would arrive one
segment late. So the newest segment is announced as an empty **growing**
file once every older segment is complete. The server's own rule then
fires one poll after the worker's, not one segment after. A growing file is
never served, because its successor does not exist until it is replaced by
the complete upload. When a worker is lost, the hub removes its growing
placeholders before `Wait` returns, so the restart path cannot mistake one
for a cached segment.

`Wait` returns only after every in-flight upload of the job has finished
and the job refuses new ones. After that the manager may delete the family:
no late write can land in a restarted family.

### 3.5 Security model

| Asset | Exposure | Notes |
|---|---|---|
| Join token and worker secrets | The join secret never crosses the wire; the worker secret crosses once, at enrollment, inside TLS and the authenticated channel | Both sides prove knowledge of `HMAC(secret, label)` over two fresh nonces. The worker refuses to send its proof to a server that cannot prove the key, so a fake server learns nothing reusable |
| Stored keys | Password-equivalent | The server stores the derived keys of join tokens and workers, not the secrets. Hashing cannot help here (HMAC needs the key). Phase 1 stores them in the worker tables like API keys |
| Transport | TLS required off loopback | The server refuses a plain listener on a non-loopback address; the worker refuses `http://` to a non-loopback host unless `--insecure`. The proofs are not bound to the TLS channel, so certificate verification matters. Decided: the join token carries the server key fingerprint, which the worker pins (§3.3) |
| Media | Per job | Streamed: only the bytes of the placed job's source, only while the job runs; never for a `shared` worker. Shared path: whatever the NFS/SMB export grants (export the library read-only) |
| Transcode directory | Per job, by name | A worker writes only the playlist, the segments and the same-stem family files of its own jobs; names are validated (no separators, no dot files); 512 MiB per file; rename into the family directory |
| API, database, users | None | A worker credential authorizes only the three `/deadfin/v1/workers/` paths, by route policy. It is not an API key |
| Scaling signals and hook | Read-only counts per pool | A separate scaling token reads only `/deadfin/v1/metrics` and `/deadfin/v1/workers/scaling`; the hook body carries the same counts and an HMAC signature. No media names, users or sessions |
| Clients | Indirect | A compromised worker can serve bad segments to the clients that play its jobs. That is the same media-parser exposure as a bad source file. Phase 4 could validate segment container headers |
| The worker host | Trusts the server | The worker runs the server's argv. Phase 1 adds the symmetric check on the worker (no absolute path except its job directory and `/dev/*`). Run the worker as an unprivileged user or container |
| Job bearer token | Visible in the worker's process list | Streamed mode passes it with `-headers`. A local loopback proxy in the agent would remove it from argv (phase 1 option) |

Revocation: revoking a join token stops new enrollments. Revoking a worker
deletes its key and closes its live connection, which loses its jobs and
restarts them elsewhere.

### 3.6 Deployment options

All options run the same binary with `deadfin worker --server URL
--token-file F --state-dir D --max-jobs N --hw-type qsv --path-map
/media=/mnt/media`. The join token is read only while `D` holds no
identity. `D` must persist (a volume or `/var/lib/deadfin-worker`); a
worker with an empty state directory and a spent join token cannot
register.

| Option | Shape | Phase |
|---|---|---|
| Container | The Deadfin image with `command: [deadfin, worker, …]`; pass `/dev/dri` or the NVIDIA runtime | 3 |
| rpm/deb | nfpm package: binary, `deadfin-worker.service`, `/etc/deadfin/worker.env`; depends on jellyfin-ffmpeg | 3 |
| Helm DaemonSet | `nodeSelector` or device-plugin resources (`gpu.intel.com/i915`, `nvidia.com/gpu`); join token from a Secret (multi-use, so every node enrolls); `hostPath` state directory per node; `emptyDir` work directory; optional read-only media volume plus `--path-map` | 3 |
| Bare binary | `deadfin worker` from a release | now |

Server side: the worker routes are on the main listener under
`/deadfin/v1/workers/`. Static tokens are in `DEADFIN_TRANSCODE_WORKER_TOKENS`
(comma-separated). A static token has the form `dfw1.<id>.<secret>.`: `<id>`
is 6 random bytes and `<secret>` is 32 random bytes, each base64url without
padding, and the last field (an optional certificate fingerprint) is empty.
Make one with:

```bash
printf 'dfw1.%s.%s.\n' "$(openssl rand 6 | basenc --base64url | tr -d =)" "$(openssl rand 32 | basenc --base64url | tr -d =)"
```

Join tokens come from the dashboard (§6). `docs/transcode-workers.md` is the
operator guide.

### 3.6.1 Stream edge (#619)

`deadfin edge` keeps HLS segments playable while the server process
restarts (D21, option B, divergence D-119).

- **Server:** `DEADFIN_EDGE_KEY` (at least 16 bytes, shared with the edge)
  adds a signed read grant (`DeadfinEdge`) to each segment URL of a media
  playlist. `DEADFIN_EDGE_URL` is the edge URL as workers reach it; with
  it, each worker job gets an upload grant for its family.
- **Worker:** mirrors each completed segment and the init segment to
  `{edge}/deadfin/v1/edge/files/{name}` before its server upload, and keeps
  mirroring while it has no server. A mirror failure never holds the
  server upload.
- **Edge:** `deadfin edge -listen :8097 -upstream http://deadfin:8096
  -store /var/cache/deadfin-edge` with `DEADFIN_EDGE_KEY`. A segment
  request with a valid grant gets the mirror copy; while the server does
  not answer (connection error, 502, 503, 504) the request waits up to
  `-wait` (15 s) for the copy, then gets 503 with `Retry-After`. Every
  other request goes to the server unchanged, WebSocket upgrades
  included. Files leave the store after `-ttl` (1 h) or past
  `-max-bytes` (20 GiB), oldest first.
- **Ingress:** route all of the server's public paths to the edge; the
  edge forwards what it does not serve. Direct streams (§3.6.2) need the
  edge to see `PlaybackInfo` and logout requests too. Workers keep dialing the server for the control channel and
  the source.
- **Replicas:** the store is per process. Run one edge replica, or
  replicas that mount one shared store directory.
- **Scope:** worker jobs only. A local transcode stops with the server
  process; the next segment request restarts it.

### 3.6.2 Direct streams through the edge (#621)

The edge also serves static direct-play streams from the media library, so
direct play continues through a server restart (divergence D-120).

- **Server:** with `DEADFIN_EDGE_KEY`, a successful `PlaybackInfo` adds
  `X-Deadfin-Edge-Direct`: one signed direct grant per library-file source
  (item, source, server path, size, container, `PlaySessionId`, 24 h
  expiry). The pinned `MediaSourceInfo` has no `DirectStreamUrl`, and
  stock clients build `/Videos/{id}/stream.{container}?static=true`
  themselves, so the grant cannot ride in a returned URL.
- **Edge:** `-library-root /media` (repeat for more roots) mounts the
  library read-only; `-path-map /media=/mnt/media` maps a server path
  prefix when the edge mounts the library elsewhere. The edge removes the
  header from the `PlaybackInfo` response and keeps the grants under the
  request's access token and the `PlaySessionId`. A later static stream
  request with the same token (header, `ApiKey` or `api_key`) or
  `PlaySessionId`, or with a direct grant in `DeadfinEdge`, is served from
  the library by range with no server request. The path must resolve,
  links followed, to a regular file below a root with the granted size;
  otherwise the request goes to the server. A logout through the edge
  forgets its token. With no `-library-root`, the edge serves no direct
  streams.
- **Kubernetes:** mount the library volume into the edge pod read-only at
  the server's path, or use `-path-map`.
- **Revocation:** the edge does not see a token revocation that does not
  pass through it, nor library permission changes. A learned grant lives
  at most 24 hours.
- **Scope:** static streams only. Remux HLS runs on a worker (§3.6.1);
  progressive transcodes are #624.

### 3.6.3 Two server replicas on PostgreSQL (#652)

A rolling update of two server replicas on one PostgreSQL store does not
interrupt playback (operator decision, option A of #652). The issue
comment records the full mechanism; divergence D-121. In short:

- **Replicas:** `DEADFIN_REPLICA_URL` (the URL at which the other replicas
  reach this process) turns on replica mode; it needs PostgreSQL. Each
  process has a random replica ID and a row in `server_replicas` with a
  15 s lease that a heartbeat renews every 5 s.
- **Owners:** `play_sessions`, `play_session_grants` and
  `transcode_worker_jobs` have an `owner` column. A row belongs to its
  owner while the owner replica is live. A replica claims a row that has
  no owner or a dead owner, and its write-behind writes only the rows it
  owns. A replica writes its session and grant changes after each local
  request that changes state, so that a peer finds the owner of a new
  `PlaySessionId` grant at once.
- **Routing:** a router wrapper keys each request on the play session of
  its client and device, on its `PlaySessionId` grant, or (worker job
  routes) on the `Deadfin-Replica` header that names the replica of the
  worker's control connection. A key that a live peer owns is forwarded to
  that peer (HTTP and WebSocket). The forward is signed with a key in
  `server_replica_secret`; the receiver takes the client address and TLS
  flag from it and answers 421 when it no longer owns the key.
- **Drain:** at SIGTERM the replica holds new routed requests, flushes the
  write-behind, releases its remote jobs as at a restart (#618), clears
  the owner of its rows and deletes its replica row. Then it forwards
  routed requests to a live peer. The peer claims each session and grant
  at its next request, and adopts each remote job when the worker
  presents it.
- **Transcode directory:** a remote job continues without a restart when
  the replicas share the transcode directory; otherwise it restarts at the
  next requested segment. A local transcode restarts on the peer at the
  requested segment.

### 3.7 Failure semantics

| Event | Detection | Effect |
|---|---|---|
| No worker fits (none registered, all full, missing hwaccel or encoder) | At placement | Local start. Same latency as today |
| Worker refuses or fails to start ffmpeg | `job.failed`, or no answer within 10 s | Local start |
| Worker process dies | TCP close, immediately | `Wait` → `ErrRunnerLost`; completed segments stay; growing placeholders are removed. The next waiting segment request restarts the family at its index, placed again by the rules (another worker or local). Measured ≈ 1 s (§5). Decided: this restart is acceptable for phase 1; no warm standby |
| Network partition, worker host hangs | Heartbeat silence > 3 × 5 s | As above, but after up to 15 s. A client waiting on a segment stalls that long. **Phase 1: a progress watchdog** (no new segment and no stderr progress for 2 × segment length while a client waits ⇒ lost) |
| ffmpeg error on the worker | `job.exited {error}` | The same as a local ffmpeg error: the request fails and the family is not retained |
| Upload fails (server full, family deleted) | PUT error | The worker stops ffmpeg and reports the exit with the error |
| Server restarts (#618) | Worker's socket closes | At shutdown the manager releases its remote families: the job records get a full lease, and the files stay. The worker keeps ffmpeg running and its new segments on disk (at most 64, then it pauses ffmpeg), stops uploads, and its source proxy retries reads. The startup wipe keeps the families of live records. On reconnect the worker sends `jobs.held`; the server adopts each job whose live record names that worker (`TranscodeManager.AdoptHLS`) and the uploads resume, or sends `job.kill`. The first request for the playlist joins the adopted family. With no reconnect the worker stops the job when the lease ends |
| `shared` worker cannot see the source | At placement (path maps), else `job.failed` | The next candidate in the rule's pool, then local |
| Seek | Manager stop: `q` on stdin, then `Kill` after the grace period | The worker stops uploading at `q`. The manager waits for `Wait`, deletes the family, and places the new start |
| Worker gets SIGTERM (scale-in, upgrade) | `drain` message | No new jobs; each job is handed off at once to another host at its first missing segment, then killed (§3.9). The client plays buffered segments meanwhile |
| No worker fits and autoscale is starting one | Placement returns "queue" | The request waits up to `queueTimeout`, then takes the fallback chain (§3.8) |
| Hardware session limit (#420) | `MaxJobs` and `-hw-sessions` per worker | A full worker is not a candidate; the job goes elsewhere or local. `-hw-sessions` bounds the jobs that use the hardware family (#622) |

### 3.8 Placement: modes, rules and the fallback chain

Decided: placement is **configurable**. A server-wide **mode** sets the
default; ordered **per-codec rules** sit on top of it; one **fallback
chain** ends every path. The operator wants to run the server on a node
without a GPU and send all transcodes to workers, so `workers-only` is a
first-class mode.

#### Modes

| Mode | No rule matches | Use |
|---|---|---|
| `local` (default) | Local, as today. Rules are not evaluated; workers take no jobs | Workers off |
| `local-first` | Local while the server runs fewer than `localLimit` transcodes; at the limit, the `defaultPools` | A capable server with overflow boxes |
| `workers-first` | The `defaultPools`; the fallback chain only when no worker fits | GPU boxes do the work; the server helps when they are full |
| `workers-only` | The `defaultPools`, with the queue (below); the fallback chain only after the queue timeout | The server is on a node without a GPU and sends every transcode out |

#### Pools and rules

A **pool** is a label on workers. A worker gets its labels from
`--pool nvenc,big-gpu` and the operator can override them in the admin
panel. A worker can be in several pools. Typical labels: `nvenc`, `qsv`,
`vaapi`, `big-gpu`, `cpu`.

Rules are ordered; the **first match wins**. Rule inputs are the facts of
the playback decision, all known before the argv is built: the source
video stream, the tone-mapping decision and the target codec. So placement
is a pure function and sits above the argv, with the phase 2 "place first"
step (§3.2):

```go
// Pure; table-tested. No store, no clock, no network.
func Place(facts JobFacts, policy Policy, fleet []WorkerSnapshot, local LocalState) Decision
```

`Decision` is one of: a worker, "queue for pool P until T", the local GPU,
the local CPU, or a refusal. The hub turns "queue" into a wait (below) and
calls `Place` again when a worker registers, frees a slot or the timeout
fires.

Schema sketch (server configuration, edited in the admin panel; shown as
YAML):

```yaml
transcodeWorkers:
  mode: workers-only            # local | local-first | workers-first | workers-only
  defaultPools: [qsv, cpu]      # where a job goes when no rule matches
  localLimit: 2                 # local-first only; 0 = never spill
  queueTimeout: 10s             # how long a job waits for a worker
  cpuFallback: true             # last resort: local software encode
  strategy: pack                # pack | spread (see autoscale, §3.9)
  rules:                        # ordered; first match wins
    - name: av1-and-10-bit-hevc-decode
      match:
        sourceCodec: [av1, hevc]          # list = any of
        bitDepth: { min: 10 }
      action: { pool: big-gpu }
    - name: tone-mapping
      match: { needsToneMap: true }
      action: { pool: [nvenc, qsv] }      # several pools = any of
    - name: dolby-vision-on-the-server
      match: { hdr: [dovi] }
      action: { local: true }
    - name: hevc-target
      match: { targetCodec: [hevc] }
      action: { pool: nvenc, queueTimeout: 20s, cpuFallback: false }
```

Match fields. Every field that is set must match (AND); a list matches any
of its values (OR); an unset field matches anything.

| Field | Values | Source of the fact |
|---|---|---|
| `sourceCodec` | `h264`, `hevc`, `av1`, `vp9`, `mpeg2video`, `vc1`, … (ffprobe codec names) | Source video stream |
| `sourceProfile` | `main`, `main10`, `high`, `high10`, … (normalized lower-case profile) | Source video stream |
| `bitDepth` | `{min, max}` | Source video stream |
| `hdr` | `sdr`, `hdr10`, `hdr10plus`, `hlg`, `dovi` | Source video range and type |
| `needsToneMap` | `true`, `false` | The playback decision: HDR source and an SDR target |
| `targetCodec` | `h264`, `hevc`, `av1`, `copy` | The playback decision |

Actions:

| Action | Meaning |
|---|---|
| `pool: <label>` or a list | A worker that carries one of the labels, has a free slot, has the capabilities the job needs (phase 2), and has an input mode that can read the source. With none: queue, then the fallback chain |
| `local: true` | Run on the server: the local GPU if the server has one, else the CPU (subject to `cpuFallback`) |
| `queueTimeout`, `cpuFallback` | Per-rule overrides of the server-wide values |

A rule that names a label no worker has ever carried is saved with a
warning, not refused: with autoscale, the pool can be empty at 0 replicas.
In the `local` mode the rules are kept but not evaluated.

#### Queue: waiting for a worker

When a job targets a pool that has no free worker, and the mode is
`workers-only` or the rule sets a `queueTimeout`, the job **queues**. The
segment request that started it waits, up to `queueTimeout`. The queue is
the scaling signal (§3.9): a queued job asks for one more slot in its pool.
When a worker of the pool registers or frees a slot, the oldest queued job
takes it. At the timeout, the job takes the fallback chain. In
`workers-first`, `local-first` and for rules without `queueTimeout`, the
timeout is 0: no wait.

The queue is in memory, owned by the hub, and ends with the request: a
client that gives up removes its job. It is not durable and survives no
restart, like every live transcode.

Default `queueTimeout: 10s`, because:

- It covers a cold start from 0 replicas with a cached image and the scale
  hook: about 2 s of pod start, 0.4 s to register, 1 s to the first
  segment (§3.9).
- It stays below the time a client waits for the first segment before it
  retries or fails. The phase 1 acceptance confirms 10 s on the slowest
  client in the ATV matrix; a retry re-enters the same queue position.
- A longer wait without a fallback looks like a hang. The warm floor
  (§3.9), not the queue, is the tool for the common case.

#### The fallback chain

Decided: every path ends in the same chain.

1. A matching worker, after the queue timeout if a worker is starting.
2. The local GPU, if the server has one (its probed `encoding.Capabilities`
   enable a hardware family).
3. The local CPU (software encode), if `cpuFallback` is on. **On by
   default**, so a household gets a slow stream rather than an error. An
   operator who prefers a clear failure to a slideshow turns it off.
4. Otherwise the request fails with the same error a failed local start
   gives today.

A job that lands on the CPU fallback is logged at `warn` with the rule, the
pool and the reason (`no-worker`, `queue-timeout`, `pool-empty`), and
increments `deadfin_transcode_fallback_total{target="local_cpu",reason}`
(§3.9). The local GPU fallback has the same counter with
`target="local_gpu"`.

### 3.9 Warm capacity and autoscale

Decided: Deadfin does not contain a cluster controller. It **exposes
scaling signals** and **tolerates scale-in**; the platform scales.

#### Signals

Per pool, computed by the hub from its live state:

| Signal | Meaning |
|---|---|
| `active_jobs` | Jobs running on workers of the pool |
| `queued_jobs` | Jobs waiting for the pool (§3.8) |
| `free_slots` | Sum of `MaxJobs − active` over the pool's registered, non-draining workers |
| `workers` | Registered workers, by state (`ready`, `draining`) |
| `wait_seconds` | Histogram of queue waits, by outcome (`placed`, `timeout`) |
| `slots_wanted` | `active_jobs + queued_jobs + warmSlots`: the one number an autoscaler targets |

`warmSlots` (per pool, default 1) is free capacity the operator wants kept
ready, so the next stream does not wait for a pod. Server-wide:
`deadfin_transcode_placements_total{target,rule}` and
`deadfin_transcode_fallback_total{target,reason}`.

Two read-only exports:

- Prometheus text format at `/deadfin/v1/metrics`, with names
  `deadfin_transcode_pool_<signal>{pool="…"}`. Deadfin has no metrics
  endpoint today; this is the first, and other subsystems can use it later.
- JSON at `/deadfin/v1/workers/scaling`, for KEDA's `metrics-api` scaler
  and for scripts:

  ```json
  {"pools":{"nvenc":{"activeJobs":3,"queuedJobs":1,"freeSlots":0,
    "workers":{"ready":2,"draining":0},"slotsWanted":5,"oldestWaitSeconds":2.4}}}
  ```

Both need an admin API key or a dedicated read-only **scaling token**, so a
scraper does not hold admin rights.

#### KEDA example

One worker Deployment per pool. `minReplicaCount` is the warm floor.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: deadfin-worker-nvenc }
spec:
  template:
    spec:
      terminationGracePeriodSeconds: 30
      containers:
        - name: worker
          image: ghcr.io/deadfin-org/deadfin:vX.Y.Z
          args: [worker, --server, https://deadfin.example, --pool, nvenc,
                 --max-jobs, "3", --token-file, /run/secrets/join, --state-dir, /state]
          resources: { limits: { nvidia.com/gpu: 1 } }
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: { name: deadfin-worker-nvenc }
spec:
  scaleTargetRef: { name: deadfin-worker-nvenc }
  minReplicaCount: 1          # warm floor; 0 = scale to zero, relies on the queue
  maxReplicaCount: 4          # number of GPUs
  pollingInterval: 5
  cooldownPeriod: 600
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:   { stabilizationWindowSeconds: 0 }
        scaleDown: { stabilizationWindowSeconds: 600,
                     policies: [{ type: Pods, value: 1, periodSeconds: 300 }] }
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus.monitoring:9090
        query: sum(deadfin_transcode_pool_slots_wanted{pool="nvenc"})
        threshold: "3"        # = --max-jobs: replicas = ceil(slots_wanted / 3)
```

The `metrics-api` trigger reads `pools.nvenc.slotsWanted` from the JSON
endpoint instead, without Prometheus. A plain HPA works through
prometheus-adapter; KEDA is simpler and can scale to 0.

Per-worker identity with replicas: the join token Secret is multi-use, and
each pod enrolls on its first start. A Deployment pod has no stable state
directory, so a replaced pod enrolls again and the old identity becomes a
stale row. Phase 3 either uses a StatefulSet (stable `state-dir` per pod)
or lets a join token mark its workers **ephemeral**: the server deletes an
ephemeral worker's identity when its connection ends.

#### Scale hook (no Kubernetes)

Optional `scaleHook: {url, secret}`. The server POSTs the JSON of one pool
when its `slotsWanted` changes (debounced to one call per second per pool)
and at once when a job queues. The body is signed with
`Deadfin-Signature: sha256=HMAC(secret, body)`. A Nomad setup calls
`nomad job scale`; a homelab script can wake a box with Wake-on-LAN and
start the worker unit. The hook only asks; the queue timeout and the
fallback chain bound what a slow or failed hook costs.

The hook also removes KEDA's polling delay, so a Kubernetes setup can use
it to call a small receiver that patches the ScaledObject or Deployment.

#### Scale-in: drain and handoff

A worker must survive being removed at any time:

1. On SIGTERM the worker sends `drain`, stops taking jobs, and keeps its
   connection open.
2. The server stops placing on it and **hands off** each of its jobs at
   once: it restarts the family at the first segment the worker has not
   completed, on another host (the rules and the chain again), exactly as
   for a lost worker (§3.7) but while the client still plays its buffered
   segments. Only when the replacement started (or failed) does the server
   send `job.kill` to the draining worker.
3. The worker exits when its jobs are gone. A live HLS job can run for
   hours, so "finish the current job" is not an option inside a grace
   period; handoff needs about one second (§5), and the default 30 s grace
   is enough.

Which pod the platform removes: a Kubernetes ReplicaSet scale-down
prefers the most recently created ready pods. So the server **packs**
(`strategy: pack`, the default when a pool autoscales): it places on the
oldest worker that has a free slot, and the newest pods stay idle and are
the ones removed. A removed busy pod is still correct (handoff), only
slower. `strategy: spread` (least-loaded first, the prototype) is better
for a fixed fleet, where it spreads heat and session limits. A later
option: the worker sets `controller.kubernetes.io/pod-deletion-cost` from
its active job count (needs RBAC to patch its own pod).

A worker that leaves without a drain (OOM kill, node loss) is a lost
worker (§3.7).

#### Cold start

Measured here (loopback, M4 Max, 5 runs): `deadfin worker` process start
to registered, ffmpeg capability probe included: **355 ms median**
(208–506 ms). First segment after placement: about 1 s (§5).

Estimated on Kubernetes (not measured; the operator's GPU-node run should
replace these):

| Step | Cached image | Uncached image |
|---|---|---|
| Demand seen (KEDA `pollingInterval: 5`; 0 with the scale hook) | 0–5 s | 0–5 s |
| HPA reaction above the minimum (sync period 15 s; 0 → 1 is KEDA itself) | 0–15 s | 0–15 s |
| Pod scheduled, GPU allocated, container started | 1–3 s | + image pull, 10–60 s |
| Worker registered | 0.4 s | 0.4 s |
| First segment | ~1 s | ~1 s |
| **Total** | **~2.5–24 s** | **~12–80 s** |

How the pieces cover it:

- **Warm floor.** `minReplicaCount ≥ 1` and `warmSlots: 1` keep one free
  slot ready, so a new stream normally places at once and starts in ~1 s,
  the same as local. Autoscale replaces the used warm slot in the
  background.
- **Queue timeout.** With a cached image and the scale hook, a cold start
  (~2.5–4 s) fits inside `queueTimeout: 10s`. KEDA polling alone can miss
  it above the minimum.
- **Fallback chain.** A cold start that misses the timeout (uncached image,
  slow scheduler) plays on the local GPU or CPU instead of failing. The
  next stream finds the new worker.
- **Pre-pull.** Phase 3 adds a DaemonSet or node image that pre-pulls the
  worker image on GPU nodes, so the uncached column is rare.
- **Early warm-up.** The playback decision (`PlaybackInfo`) arrives a
  second or more before the first segment request. Phase 2 can count a
  pending decision in `slotsWanted` to start the scale-up earlier.

Built in #623:

- `deadfin worker -ephemeral` (`DEADFIN_WORKER_EPHEMERAL`): the worker keeps
  no state directory, enrolls with the join token at each start and says
  `ephemeral` in its hello. The server deletes an ephemeral identity when
  that connection ends, but not when the server itself stops, so workers
  reconnect to the next server process. A static token is never ephemeral.
- The scale hook (`DEADFIN_TRANSCODE_SCALE_HOOK_URL`,
  `DEADFIN_TRANSCODE_SCALE_HOOK_SECRET`; a URL without a secret warns and
  stays off). The body also has a call for all pools (`"pool":"*"`); the
  reason is `queued` for a call at once and `changed` for a debounced one.
- The warm-up: a PlaybackInfo response with a `TranscodingUrl` counts its
  PlaySessionId in `warmingJobs` and `slotsWanted` of the default pools
  until a job starts for it or for one minute, and wakes the scale hook.
- Packaging: the Helm chart `deploy/charts/deadfin-worker` (DaemonSet with
  durable identities, or a Deployment per pool with ephemeral identities
  and a KEDA ScaledObject, plus the pre-pull DaemonSet), the nfpm package
  `deploy/packaging` (systemd unit, `/etc/deadfin/worker.env`), and the
  checks `tools/worker-smoke` (Compose) and
  `deploy/packaging/package-smoke`. Operator guide:
  `docs/transcode-workers.md`.

The prototype has none of §3.8 and §3.9. It places every eligible HLS
family on the least-loaded worker that fits and falls back to local; on
SIGTERM the worker stops its jobs (the server sees a loss, not a drain).

## 4. Prototype

| Path | What |
|---|---|
| `internal/ffmpeg/runner.go` | `RemoteRunner`, `RemoteProcess`, `ErrNoRemoteRunner`, `ErrRunnerLost`; the manager's `process` interface |
| `internal/ffmpeg/remote_start.go` | `startRemote`: offers the family, wires stderr and stdin, falls back locally |
| `internal/ffmpeg/exec.go` | `Exec`: one ffmpeg in its own process group, for the worker |
| `internal/ffmpeg/transcode_manager.go`, `hls_job.go` | `*exec.Cmd` → `process`; `WaitSegment` reports `ErrRunnerLost` |
| `internal/playback/hls_service.go` | One restart at the segment after `ErrHLSRunnerLost` |
| `internal/transcodeworker/` | Protocol, `Hub` (server, implements `RemoteRunner`), `Agent` (worker) |
| `cmd/deadfin/transcode_workers.go` | `deadfin worker` and the server-side worker wiring |
| `tools/transcodeworker-demo/` | The two-process measurement harness |

Decided items that the prototype does not build (the doc is the spec for
phase 1): the main-listener routes, join-token enrollment and worker
identities, fingerprint pinning, the input setting (the prototype is
`auto`), the placement modes, rules, queue and fallback chain, the scaling
signals, and drain on SIGTERM. The demo now also reports the worker's
process-start-to-registered time (§3.9).

Conventions: context first; no package-level mutable state; the hub owns
one expiry goroutine and waits for its connection handlers in `Close`; each
agent session owns its writer, heartbeat and job goroutines and waits for
them before it returns; `Exec` owns its wait goroutine. All timers use
`clock.Clock`; the tests use `clock.Fake` and `goleak`, with no sleeps. No
tables (tokens come from the environment).

Tests:

- `internal/transcodeworker`: token parsing and proof symmetry; argv rewrite
  and refusal of server-local resources; registration (good token, wrong
  secret, unknown token, bad proof); heartbeat expiry on a fake clock; job
  flow (ranged source, stderr, growing then complete segments, stdin,
  final segment before `Wait`, closed data plane after `Wait`); shared-path
  input; worker loss mid-stream with a restart on the surviving worker;
  session limits, hwaccel matching and kill; artifact name and token
  checks; plain HTTP refused off loopback; hub close.
- `internal/ffmpeg`: a remote family behind the unchanged manager contract;
  mid-stream loss, retained segments and a local restart at the requested
  segment; local fallback; no offer for burn-in; canonical operands under a
  symlinked transcode directory.
- `internal/playback`: one restart after `ErrHLSRunnerLost`, and no second.
- `cmd/deadfin`: off unless configured; TLS required off loopback.

## 5. Measurements

Two processes on one Mac: the demo process is the server (a real
`TranscodeManager` and `Hub` on loopback); `deadfin worker` is the second
process. The harness drives the manager the way `HLSService` does.

- Host: Apple M4 Max, 16 cores; ffmpeg 8.1.2 (nix).
- Source: 180 s, 1080p24 H.264 at 10 Mb/s (`testsrc2` + noise), 5.1 AC-3,
  236 MiB, MKV.
- Output: 720p libx264 `veryfast`, 4 Mb/s, AAC stereo, 6 s MPEG-TS segments,
  live (event) playlist. Software encoding in every mode.
- Five runs per mode; medians. "First segment" = StartHLS + playlist with
  one entry + segment 0 body. "Seek" = segment 15 requested after segment 3
  (kill + restart with `-ss 90`). "Failover" = from `SIGKILL` of the worker
  to the body of the first segment it had not completed (local restart).

```
go build -o deadfin ./cmd/deadfin
go build -o demo ./tools/transcodeworker-demo
./demo -media movie.mkv -worker ./deadfin -modes local,stream,shared -runs 5
```

| Mode | First segment | Second segment | Seek to segment 15 | Failover (worker SIGKILL) | Source over HTTP | Uploaded | Playlist hash |
|---|---|---|---|---|---|---|---|
| local | 1.014 s | 405 ms | 1.309 s | n/a | 0 | 0 | 022366751ad1 |
| stream | 910 ms | 304 ms | 1.284 s | 1.017 s | 85.9 MiB | 15.4 MiB | 022366751ad1 |
| shared | 1.217 s | 405 ms | 1.798 s | 2.036 s | 0 | 15.4 MiB | 022366751ad1 |

Run ranges: local first segment 811–1119 ms; stream 910–1014 ms; shared
1.01–1.32 s. Seek: local 1.27–1.60 s, stream 1.26–1.39 s, shared
1.51–2.83 s.

Reading:

- **Playlist unchanged.** The first three playlist entries hash the same
  in all modes; the client playlist is a pure rewrite of that text.
- **Loopback overhead is below the poll quanta.** The manager polls every
  100 ms and the worker polls every 100 ms, so remote results land on a
  ~100 ms grid. Stream mode was not slower than local. The worker is a
  separate process, so there is no extra CPU contention to explain.
- **Shared mode was slower for seek and failover** (1.8 s and 2.0 s
  medians) with a wide spread. The cause is not found. Both modes run the
  same argv; only the input operand differs. Re-measure on real NFS before
  drawing a conclusion.
- **Streamed source read-ahead.** For about 42 s of encoded media (segments
  0–3, then 15–17 after the seek) ffmpeg read 86 MiB over HTTP: about 3× the
  bytes at 10 Mb/s. The cost per stream is the source bitrate times that
  factor. On a LAN this is fine; for an 80 Mb/s 4K remux on a WAN link it is
  not, and shared-path mode should be preferred there.
- **Upload is 1× the output.** 15.4 MiB, the HLS output itself.
- **Not measured here:** real network latency. Expected added time on the
  critical path per segment: one upload (a 3 MB segment takes ~25 ms on
  1 GbE, ~250 ms on 100 Mb/s) plus one poll. For the first segment add one
  control round trip for placement and the few range requests of ffmpeg's
  probe. The operator's GPU-node run should repeat this table with
  `-hwaccel` workers on the real network.

## 6. Phased plan

Each phase is one issue, one branch and one pull request. The phases form
their own milestone after v0.3.0.

### Phase 0 — this spike (done)

Seam, protocol, hub, agent, env gate, demo, tests. Acceptance: Gate A green;
the §5 table.

### Phase 1 — managed software workers

- Worker endpoints on the main listener under `/deadfin/v1/workers/`, with
  a `worker` route policy (§3.3). Remove the experimental listener.
- Store: `transcode_worker_join_tokens` (id, derived key, fingerprint,
  expiry, uses left, created, revoked) and `transcode_workers` (id, name,
  join token id, derived key, input mode, created, last seen, revoked).
  SQLite and PostgreSQL migrations together; all SQL in `internal/store`.
- Enrollment: join token → per-worker identity; `--state-dir` identity file;
  fingerprint pinning and the `pins` rotation message (§3.3).
- Per-worker input mode `shared | stream | auto`; path-map prefixes in the
  registration; the data plane refuses the source to a `shared` worker.
- Placement (§3.8): the four modes, pool labels (`--pool`, admin
  override), the rule schema and validation on save, the pure `Place`
  function, the queue with `queueTimeout`, and the fallback chain with
  `cpuFallback` (on by default). In phase 1 the argv is still the
  server's, so a pool moves the server's argv only; phase 2 makes pools
  hardware-aware.
- Drain and handoff on SIGTERM (§3.9); `strategy: pack | spread`.
- Signals (§3.9): `/deadfin/v1/metrics` (Prometheus text) and
  `/deadfin/v1/workers/scaling` (JSON), with a read-only scaling token.
- Native admin API under `/deadfin/v1/transcode-workers` (D13): create a
  join token (shown once, with the fingerprint), list workers with
  capabilities and load, set the input mode, revoke a token, revoke a
  worker (closes the live connection), drain (no new jobs), read and write
  the pools and rules. Admin policy on route registration. deadfin-web:
  Dashboard → Playback → Transcode workers.
- Failover: the measured ~1 s restart is accepted; no warm standby. Add the
  progress watchdog (§3.7) so a partition costs the same restart, not 15 s.
- Worker-side argv path check; job token out of argv (agent loopback
  proxy).
- Metrics: the §3.9 pool signals, placements by rule, fallbacks by target
  and reason, losses, upload bytes and latency.

Acceptance:
- storetest: both worker tables on both backends.
- API tests: create token, enroll, list, set input mode, revoke token (no
  new enrollment, enrolled workers stay), revoke worker (connection closed),
  drain, rules round trip and refusal of an unknown pool, admin-only.
- `internal/transcodeworker`: enrollment writes an identity and the next
  connection uses it; a wrong fingerprint is refused before the handshake;
  a pin update survives a rotation; a `shared` worker is not offered a
  source outside its maps and its source GET is refused; revoke drops the
  connection and loses its jobs; the watchdog on a fake clock declares a
  stalled job lost; drain stops placements.
- `Place` table tests: each mode with no rule matching (`local-first` with
  `localLimit` 0, below and at the limit); first match wins; every match
  field; AND across fields and OR within a list; several pools in one
  action; per-rule `queueTimeout` and `cpuFallback` overrides; the chain
  order worker → local GPU → local CPU → refusal; `pack` places on the
  oldest worker with a free slot, `spread` on the least loaded.
- Queue, on a fake clock: **`workers-only` with 0 workers warm**, a
  playback request queues, a worker registers before the timeout, and the
  job runs on it (the wait is recorded in `wait_seconds{outcome="placed"}`);
  **the queue timeout path**: no worker arrives, the job takes the chain
  at exactly `queueTimeout` (`wait_seconds{outcome="timeout"}`); a client
  that cancels removes its queued job.
- **CPU fallback**: `workers-only`, 0 workers, a server without a GPU, the
  job runs on the software encoder after the timeout,
  `deadfin_transcode_fallback_total{target="local_cpu",reason="queue-timeout"}`
  increments by 1, and one `warn` log names the reason. With
  `cpuFallback: false` the same request fails with the local start error
  and nothing runs.
- **Drain on SIGTERM mid-job**: a worker with a running job gets SIGTERM;
  it sends `drain`, receives no new placement, its job restarts on a second
  worker at the first segment it had not completed, the old job gets
  `job.kill` only after that start, the client's next segment is served,
  and the worker process exits within the grace period with no goroutine
  leak.
- **Metric values under load**: with two workers (`MaxJobs` 2 each),
  `warmSlots` 1 and five concurrent playbacks, the scaling JSON and the
  Prometheus text report `active_jobs` 4, `queued_jobs` 1, `free_slots` 0,
  `slots_wanted` 6; after two playbacks stop, `active_jobs` 3,
  `queued_jobs` 0, `free_slots` 1, `slots_wanted` 4. Both exports agree,
  and the scaling token cannot read any other path.
- Client timeout check (ATV matrix): a 10 s queue wait on the slowest
  client in the matrix still starts playback.
- A CI integration test with two processes on the main listener (server +
  `deadfin worker`, software, a 10 s fixture, self-signed certificate with
  a pinned fingerprint): playlist identical to local; SIGKILL of the
  worker mid-stream serves the next segment within 2 s.

### Phase 2 — hardware placement

- Workers report their full `encoding.Capabilities` (probe on the worker:
  VAAPI driver, NVENC encoders that open, tonemapx, versions).
- Placement before argv: `playback` evaluates the rules, asks the runner
  for a worker in the chosen pool, and builds the argv with that worker's
  capabilities. Now "AV1 decode to the GPU pool" and "tone mapping to the
  GPU pool" use the worker's hardware.
- Per-family session limits (for example NVENC sessions per GPU, #420),
  and device-node mapping.

Acceptance:
- Unit: argv for a QSV placement on a server without a GPU; a tone-mapping
  rule places on a worker that reports `tonemapx` and builds its filter
  chain; a full NVENC worker falls back to a QSV worker in the same pool,
  then to local software.
- Operator run on real GPU nodes: the §5 table for QSV, VAAPI and NVENC
  workers, and the ATV matrix cells for hardware transcodes through a
  worker.

### Phase 3 — packaging and autoscale

Container command, nfpm rpm/deb with a systemd unit, Helm chart (a
DaemonSet for a fixed fleet, or one Deployment plus a KEDA ScaledObject per
pool, §3.9), ephemeral worker identities for Deployment replicas, the scale
hook, an image pre-pull DaemonSet, early warm-up from `PlaybackInfo`,
install docs.

Acceptance: image smoke runs a worker against a server (compose) and plays
one segment; `helm template` and `helm lint` in CI for both chart shapes;
the package installs and the unit starts in a container test; the scale
hook is called once when a job queues and is signed (fake receiver, fake
clock); an ephemeral worker's identity is gone after it disconnects. On
the operator's cluster: a `workers-only` playback with `minReplicaCount: 0`
measures pod start to first segment (the §3.9 cold-start table), and a
scale-in from 2 to 1 replicas while one stream plays keeps that stream
playing.

### Phase 4 — the rest of the transcode surface

Subtitle burn-in (ship the extracted subtitle and fonts as job assets over
the data plane), progressive transcodes (stream the output back),
segment header validation.

Built in #624:

- Burn-in: `RemoteHLSJob.Assets` carries the pinned subtitle (external or
  extracted) and the regular files of its fontsdir (at most 256 fonts and
  256 MiB, else the job stays local). The argv names them under
  `ffmpeg.RemoteAssetsPlaceholder`; the worker fetches each from
  `GET …/jobs/{id}/assets/{name}` before ffmpeg starts, into
  `assets/subtitle.<ext>` and `assets/fonts/`, and replaces the
  placeholder with its escaped asset directory. A burn-in whose
  `subtitles=` filter reads the media file itself (no extraction) stays
  local.
- Progressive: `ffmpeg.RemoteProgressiveRunner`. The worker streams the
  growing output in chunks of at most 8 MiB to `POST …/jobs/{id}/output`
  with `Deadfin-Output-Offset`; the hub appends at that offset (409 with
  its own size on a mismatch) and the manager's reader tails the file as
  for a local ffmpeg. A progressive job has no lease, edge mirror or
  handoff: a lost worker ends that response, and the client's next request
  (a seek) starts a new job. A progressive burn-in still names server
  paths and stays local.
- Checks: `hls.CheckFamilyFile` on the server and on the edge. A `.ts`
  file is whole 188-byte packets that each start with 0x47; a `.mp4` or
  `.m4s` file is ISO BMFF boxes with printable types that cover the file
  and open with `ftyp`, `styp`, `moof`, `sidx`, `emsg` or `prft`; a playlist
  is UTF-8 text from `#EXTM3U`, at most 4 MiB. A malformed upload answers
  422; on the server it loses the job (the family restarts elsewhere) and
  quarantines the worker.

### Later — background jobs over the same queue (not built)

Trickplay generation, subtitle extraction, keyframe extraction, audio
normalization and media-segment analysis are batch jobs with files as
inputs and outputs. They fit the Tdarr model better than live jobs do: a
job queue with retries, no latency target, and the same worker,
capabilities, token and data plane. The scheduler (`internal/tasks`) would
place a task through the same hub.

## 7. Divergence row (proposal)

Landed as D-118 with #614. The proposal as written in the spike:

| ID | Divergence | Why | User-visible consequence | Corpus impact |
|---|---|---|---|---|
| D-64 | Remote transcode workers: when an operator registers worker hosts, Deadfin may run a live HLS ffmpeg family on a worker (`ffmpeg.RemoteRunner`). The worker dials out, reads the source by shared path or an authenticated range URL, and uploads the playlist family into the server's transcode directory through
`/deadfin/v1/workers/` on the main listener. Operator rules choose local or
a worker pool per job. Pinned Jellyfin runs every ffmpeg as a local child (`TranscodeManager.StartFfMpeg`) | Moves transcode load off the server host and onto GPU boxes without shared transcode storage or SSH keys (rffmpeg's model) | None on the wire: the same playlists, segment URLs and readiness rule. A worker loss mid-stream costs one restart at the requested segment, about the cost of a seek. Off unless workers are registered | n/a (no corpus step registers a worker) |

## 8. Risks

- **Two writers by mistake.** The aliasing bug in §3.3 is the class to
  watch: any operand the hub does not rewrite must refuse the job. Keep
  the refusal rule strict.
- **Restart storms.** A flapping worker could take, lose and retake jobs.
  Phase 1: a short quarantine for a worker that loses a job.
- **Argv drift.** Phase 1 runs the server's argv on another ffmpeg build.
  Require the same jellyfin-ffmpeg major version at registration.
- **Clients and stalls.** A partition stalls a client up to the heartbeat
  limit until the watchdog lands.
- **Pinned key rotation.** A server key change without a `pins` update
  strands every pinned worker. The admin panel must show the pin set and
  warn before the certificate changes.
- **Rule misconfiguration.** `cpuFallback: false` on a broad rule can
  refuse playback when the pool is down. Show the rule that placed each session
  in the dashboard.

## 9. Decisions

The operator answered the open questions on #538 (issuecomment-43027).

| # | Question | Decision | Where |
|---|---|---|---|
| 1 | Scope | Its own milestone after v0.3.0 | §6 |
| 2 | Listener | The main listener, under `/deadfin/v1/workers/` | §3.3 |
| 3 | Tokens | One join token that mints a revocable per-worker identity | §3.3 |
| 4 | Server identity | The join token embeds the server certificate fingerprint | §3.3, §3.5 |
| 5 | Placement | Configurable: a server-wide mode (`local`, `local-first`, `workers-first`, `workers-only`) plus ordered per-codec rules (first match wins; action: a pool label or local). Workers carry pool labels | §3.8 |
| 5a | No worker available | A queue with `queueTimeout` (default 10 s) while autoscale starts a worker, then the chain: worker → local GPU → local CPU → error. The CPU step is a setting, on by default; a CPU fallback is logged and counted | §3.8 |
| 5b | Warm capacity and autoscale | No cluster controller in Deadfin. Per-pool scaling signals (Prometheus, JSON), a KEDA example with `minReplicaCount` as the warm floor, an optional scale hook, drain and handoff on SIGTERM, pack placement so scale-in removes idle pods | §3.9 |
| 6 | Input | A per-worker setting: `shared`, `stream` or `auto` | §3.3 |
| 7 | Restart on loss | The ~1 s restart is acceptable for phase 1; no warm standby | §3.7, §6 |

Still open:

1. The default mode for a new install: `local` (workers do nothing until
   the operator picks a mode) is assumed.
2. The `queueTimeout` default of 10 s is a judgment (§3.8); the phase 1
   ATV check confirms or lowers it.
3. A metrics endpoint is new to Deadfin. Write the Prometheus text format
   by hand for these few series, or add `prometheus/client_golang`?
