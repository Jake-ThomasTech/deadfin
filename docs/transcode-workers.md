# Transcode workers

A transcode worker runs the server's transcodes on another host: live HLS
families, subtitle burn-ins and progressive transcodes.
It is the same `deadfin` binary, started as `deadfin worker`. The worker
dials out to the server, so the worker host needs no open port.

Live TV transcodes read the server's stream broker, so they run on the
server.

The default placement mode is `local`: with no worker, every transcode runs
on the server, as in Jellyfin. Workers, the scale hook and the warm-up are
each optional.

Design: `docs/spikes/transcode-workers.md`. Divergences: D-118, D-121.

## 1. Prepare the server

Choose one way to give the worker a credential.

- **Static token (GitOps).** Make a token and put it in the server
  environment. Several tokens are comma-separated.

  ```bash
  printf 'dfw1.%s.%s.\n' "$(openssl rand 6 | basenc --base64url | tr -d =)" "$(openssl rand 32 | basenc --base64url | tr -d =)"
  ```

  ```
  DEADFIN_TRANSCODE_WORKER_TOKENS=dfw1.…
  ```

- **Join token (dashboard).** Create a join token in the dashboard
  (Transcode workers, Add worker). Each worker that uses it enrolls its own
  revocable identity. A multi-use join token lets every node of a fleet
  enroll.

Then set the placement mode (also in the dashboard):

```
DEADFIN_TRANSCODE_WORKER_MODE=workers-first   # local, local-first, workers-first, workers-only
```

Inside the pools that the mode and the rules choose, placement takes the
worker whose argv runs the most of the job in hardware: tier 2 decodes and
encodes in hardware, tier 1 does one of them, tier 0 is software. The
strategy (`pack` or `spread`) breaks a tie. An AV1 source goes to a worker
that decodes AV1 in hardware before one that decodes it in software. Each
placement logs its `tier`.

In a mode other than `local`, the HLS output codec is the first codec of
the client's list that the server or a ready worker can encode, after
`AllowHevcEncoding` and `AllowAv1Encoding`. A codec that only a worker
encodes runs on such a worker only: when none is free, the job waits up to
`queueTimeout` (10 s by default) and then fails. A play session keeps its
codec; a new play session chooses again. The server logs each choice with
its source, `server` or `worker:<id>`.

Workers connect over TLS. On a trusted network, plain HTTP needs the
server opt-in and `-insecure` on the worker. Turn on the server opt-in in
the dashboard (transcode workers page), or set
`DEADFIN_TRANSCODE_WORKERS_ALLOW_HTTP=true`. The environment variable wins
when it is set, `true` or `false`. A dashboard change applies to the next
worker request without a restart; a worker that is already connected stays
connected. The server logs a warning when plain HTTP turns on.

The dashboard setting is `AllowPlainHttpTranscodeWorkers` in the encoding
configuration (`POST /System/Configuration/encoding`).
`GET /deadfin/v1/transcode-workers/plain-http` returns the value in effect:

```json
{"AllowPlainHttp": true, "Configured": true, "SetByEnvironment": false}
```

The worker's ffmpeg major version must equal the server's. Use the same
Deadfin version, or jellyfin-ffmpeg of the same major version, on both.

A worker keeps its jobs through a server restart for the job lease
(`DEADFIN_TRANSCODE_JOB_LEASE`, 60 s). For the client to keep its play
session through the restart, turn on the play session reload:

```
DEADFIN_PLAY_SESSION_RESTORE=true
```

The stream edge (`DEADFIN_EDGE_KEY`) and server replicas
(`DEADFIN_REPLICA_URL`) turn it on. With none of these, play sessions live
in memory only, as in Jellyfin.

## 2. Install a worker

### Container

```bash
docker run -d --name deadfin-worker --restart unless-stopped \
  -e DEADFIN_WORKER_TOKEN=dfw1.… \
  -v deadfin-worker:/state --device /dev/dri \
  ghcr.io/deadfin-org/deadfin:<version> \
  worker -server=https://deadfin.example -state-dir=/state \
  -max-jobs=3 -hw-type=qsv -pool=qsv
```

For NVIDIA, replace `--device /dev/dri` with `--gpus all` and use
`-hw-type=nvenc -hw-sessions=<session limit>`.

### Debian or RPM package

The `deadfin-worker` package holds the binary, `deadfin-worker.service` and
`/etc/deadfin/worker.env`. It depends on `jellyfin-ffmpeg8` from the
Jellyfin repository.

1. Install jellyfin-ffmpeg8, then the package.
2. Edit `/etc/deadfin/worker.env`: set `DEADFIN_WORKER_SERVER`,
   `DEADFIN_WORKER_TOKEN` and `DEADFIN_WORKER_ARGS`.
3. Run `systemctl enable --now deadfin-worker`.

The identity is in `/var/lib/deadfin-worker`. The unit user joins the
`render` and `video` groups that exist at install time.

To build the package: put the linux binary at `dist/deadfin`, then run
`DEADFIN_VERSION=0.4.0 DEADFIN_ARCH=amd64 nfpm package --config
deploy/packaging/nfpm.yaml --packager deb` (or `rpm`).

### Kubernetes (Helm)

The chart is `deploy/charts/deadfin-worker`. It has two shapes.

**Fixed fleet** (`mode: daemonset`): one worker per selected node. Each
worker keeps a durable identity in a `hostPath` state directory.

```yaml
server: { url: https://deadfin.example }
token: { existingSecret: deadfin-worker-join }   # key "token"
mode: daemonset
nodeSelector: { intel.feature.node.kubernetes.io/gpu: "true" }
worker: { maxJobs: 3, hwType: qsv }
daemonset: { pools: [qsv] }
```

**Autoscaled** (`mode: deployment`): one Deployment per pool, optionally
scaled by KEDA. Replicas are **ephemeral** workers: each pod enrolls with
the join token when it starts, and the server deletes its identity when its
connection ends. A server restart keeps the identities, so workers reconnect
to the next server process.

```yaml
server: { url: https://deadfin.example }
token: { existingSecret: deadfin-worker-join }
mode: deployment
gpu: { mountDri: false, runtimeClassName: nvidia }
pools:
  - name: nvenc
    maxJobs: 3
    hwType: nvenc
    resources: { limits: { nvidia.com/gpu: 1 } }
keda:
  enabled: true
  minReplicaCount: 1        # warm floor; 0 scales to zero
  maxReplicaCount: 4
  trigger: metrics-api      # or prometheus
  metricsApi: { tokenSecret: deadfin-scaling-token }
prePull: { enabled: true, nodeSelector: { nvidia.com/gpu.present: "true" } }
```

The ScaledObject targets `slotsWanted` of the pool with `maxJobs` jobs per
replica. The `metrics-api` trigger reads `/deadfin/v1/workers/scaling` with
a scaling token (dashboard, or `DEADFIN_TRANSCODE_SCALING_TOKENS`). The
`prometheus` trigger queries `deadfin_transcode_pool_slots_wanted`.

`prePull` runs a DaemonSet that keeps the worker image on the GPU nodes, so
a scale-up does not wait for an image pull.

A removed pod drains: on SIGTERM the server hands its jobs off to another
host, and playback continues. Keep `terminationGracePeriodSeconds` above
`worker.drainGrace`.

## 3. Scale hook

Without Kubernetes, or to remove the KEDA polling delay, the server can call
a URL when it needs capacity:

```
DEADFIN_TRANSCODE_SCALE_HOOK_URL=https://scaler.example/deadfin
DEADFIN_TRANSCODE_SCALE_HOOK_SECRET=<16 bytes or more>
```

The server POSTs one JSON body per pool, and one for all pools (`"*"`):

```json
{"pool":"nvenc","reason":"queued","signals":{"activeJobs":3,"queuedJobs":1,"warmingJobs":0,
  "freeSlots":0,"workers":{"ready":1,"draining":0},"slotsWanted":5,"oldestWaitSeconds":0.2}}
```

- `reason` is `queued` when a job waits for a worker or a playback is about
  to start a transcode. The call comes at once.
- `reason` is `changed` when `slotsWanted` changes otherwise. These calls
  come at most once per second per pool.
- The `Deadfin-Signature` header is `sha256=` and the hex HMAC-SHA256 of the
  body with the secret. Verify it before you act.

A URL without a secret is ignored, with one warning at startup.

## 4. Warm-up from PlaybackInfo

When a PlaybackInfo response announces a transcode, the server counts it
in `warmingJobs` and `slotsWanted` of the default pools until its job starts
or for one minute. The autoscaler starts a worker before the client asks for
the first segment. Placement mode `local` counts no warm-up.

## 5. Two server replicas

On PostgreSQL, two or more server replicas can run at once, so that a
rolling update does not interrupt playback (D-121).

- Set `DEADFIN_REPLICA_URL` on each replica to the URL at which the other
  replicas reach it, for example `http://$(POD_IP):8096` from the Kubernetes
  downward API. Keep that address on a private network.
- Give the replicas one transcode directory (one shared volume for the
  program data or for `cache/transcodes`). A remote HLS job then moves to
  the next replica with no restart.
- Use a Deployment with `strategy: RollingUpdate`, `maxSurge: 1` and
  `maxUnavailable: 0`, and a `terminationGracePeriodSeconds` of at least 30.
- Point the workers and the clients at the Service.

A play session belongs to the replica that first serves its device, and a
`PlaySessionId` grant and a remote job to the replica that made them. The
other replicas forward the requests of that session, grant or job to the
owner. At SIGTERM a replica hands its sessions, grants and remote jobs to
the other replicas and then stops.

## 6. Checks

- `tools/worker-smoke IMAGE` runs a server and a worker with Docker Compose
  and plays the first segment of a transcode through the worker.
- `deploy/packaging/package-smoke IMAGE` builds the deb, installs it in a
  Debian container that runs systemd, and starts the unit.
- The `charts` workflow runs `helm lint` and `helm template` for both chart
  shapes (`deploy/charts/deadfin-worker/ci/`).
- `TestRollingUpdateOfTwoReplicas` drains one of two replicas while a
  client plays through a worker process (PostgreSQL with
  `DEADFIN_TEST_POSTGRES_DSN`).
