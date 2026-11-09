# Reference architectures

Two tested layouts for running Deadfin at home: one server, and two
servers with a floating address. Both serve clients on
`https://media.example.com:8920`, as [install.md](install.md#external-access)
describes. Addresses below are examples; use your own.

| | Single node | Two-node HA |
|---|---|---|
| Hosts | 1 | 2 |
| Database | SQLite or PostgreSQL | PostgreSQL (required for replicas) |
| Shared storage | none | data root and transcode directory on NFS, both hosts read-write |
| TLS proxy | none on a LAN; **required** for remote access (Caddy on the same host) | Caddy on both hosts, one shared certificate |
| Client address | the host | a floating IP (keepalived) |
| Survives | nothing: a host restart stops playback | a proxy or host failure, and a rolling update (running transcodes only with a transcode worker) |
| Minimum version | any | v0.4.0-rc.1 (server replicas, D-121) |

## 1. Single node

```
clients ──:8920──▶ router port forward ──▶ host
                                           ├─ Caddy :8920 (TLS, DNS-01)
                                           └─ Deadfin :8096 (GPU, /dev/dri)
                                                │
                              media (read-only) ┘  NAS or local disk
```

One host runs Deadfin, plus Caddy when clients connect from outside your
network.

> [!WARNING]
> **External access needs a TLS reverse proxy.**
> Deadfin serves plain HTTP on 8096 and doesn't terminate TLS itself.
> **Never forward 8096 to the internet**: passwords and session tokens
> would cross it in clear text, and the setup page would be exposed
> ([deployment-security.md](deployment-security.md#exposure-decision)).
>
> - **LAN only:** no proxy needed. Clients use `http://<host>:8096`.
> - **Remote access, your own devices:** a VPN (WireGuard, Tailscale) is
>   the smallest exposure. Nothing is published.
> - **Remote access for others or for TV apps:** put a maintained reverse
>   proxy with HTTPS in front and forward only its port (8920). Caddy is
>   shown here because it gets and renews the certificate by itself;
>   nginx or Traefik work the same way.

The rest of this section assumes remote access through Caddy.

- **Database:** SQLite in the data root is enough for one process. Use
  PostgreSQL if you plan to add a second server later; moving a library
  between stores needs a fresh install and a new import.
- **Media:** mount it read-only. Deadfin doesn't write to media folders.
- **GPU:** pass `/dev/dri` through and add the group that owns
  `/dev/dri/renderD128` on the host (`video` or `render`, check with
  `ls -ln /dev/dri`) with `group_add`.
- **TLS:** Caddy on port 8920 with the ACME DNS challenge, because port 80
  stays closed. Build Caddy with your DNS provider's plugin
  (`xcaddy build --with github.com/caddy-dns/<provider>`).
- **Proxy address:** list the address Deadfin sees for Caddy in
  `KnownProxies`. With Caddy in the same Compose project, that is the
  Compose network's subnet, for example `172.18.0.0/16`. Check the address
  in Dashboard → Sessions after you sign in through the proxy: it must show
  your client, not the proxy.
- **Backups:** the data root (`/var/lib/deadfin`), plus the database if it
  is PostgreSQL.

Example `compose.yaml`:

```yaml
services:
  deadfin:
    image: ghcr.io/deadfin-org/deadfin:v0.4.0-rc.1@sha256:<digest>
    restart: unless-stopped
    env_file: .env                # TMDB_API_KEY, optional DEADFIN_DATABASE_URL
    devices:
      - /dev/dri:/dev/dri
    group_add:
      - "44"                      # the group that owns renderD128 on this host
    volumes:
      - ./data:/var/lib/deadfin
      - /mnt/media:/mnt/media:ro
  caddy:
    image: <your caddy build with caddy-dns/<provider>>
    restart: unless-stopped
    ports:
      - "8920:8920"
    env_file: caddy.env           # DNS provider API token
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
volumes:
  caddy-data:
```

`Caddyfile`:

```
{
	auto_https disable_redirects
}

media.example.com:8920 {
	tls {
		dns <provider> {env.DNS_API_TOKEN}
	}
	reverse_proxy deadfin:8096 {
		flush_interval -1
	}
}
```

## 2. Two-node HA

```
clients ──:8920──▶ router port forward ─┐
LAN clients (local DNS) ────────────────┤
                                        ▼
                    floating IP 10.0.60.10 (keepalived, DMZ VLAN)
            ┌───────────────────────┴───────────────────────┐
  node A (preferred)                               node B
  Caddy :8920 on the floating IP                   Caddy :8920 on the floating IP
  Deadfin replica  :8096                           Deadfin replica  :8096
            │      └──── replica traffic, private network ────┘      │
            └──────────── PostgreSQL (primary/replica) ──────────────┘
         shared data root + transcode directory (NFS)   media (read-only NFS)
```

Two hosts each run a Deadfin replica and Caddy. keepalived moves one
floating IP between them. Clients and transcode workers use only the
floating IP's name.

### Network

- Give each host two networks: a **client-facing** network (we use a DMZ
  VLAN) for Caddy and the floating IP, and a **private** network for the
  database, NFS and replica traffic.
- If the client-facing network has its own gateway, add source-based
  routing so replies from it leave through that gateway, not the default
  route (otherwise the router drops them as asymmetric). With netplan:

  ```yaml
  vlans:
    bond0.60:
      id: 60
      link: bond0
      addresses: [10.0.60.11/24]
      routes:
        - {to: default, via: 10.0.60.1, table: 60}
        - {to: 10.0.60.0/24, scope: link, table: 60}
      routing-policy:
        - {from: 10.0.60.0/24, table: 60, priority: 100}
  ```

  The rule covers the floating IP too, because it is in the same subnet.

- On Broadcom NetXtreme-E NICs (`bnxt_en`), run a kernel with upstream fix
  `a51d233ccd48` (*bnxt_en: fix DMA mapping length for padded small
  packets*: Linux 7.3, 7.2.y, 6.18.y, 6.12.y; Ubuntu 26.04 from
  `7.0.0-43`) before sending traffic on a VLAN interface. Without it,
  short frames such as keepalived's VRRP adverts trigger IOMMU faults
  (`DMAR: [DMA Read NO_PASID] … PTE Read access is not set`), and the NIC
  stops transmitting.

### Database

- One PostgreSQL database for both replicas. A primary/replica pair works
  through a multi-host URL, so no VIP or HAProxy is needed in front of it:

  ```
  DEADFIN_DATABASE_URL=postgres://deadfin:<pw>@10.0.50.62:5432,10.0.50.63:5432/deadfin?target_session_attrs=read-write
  ```

  `target_session_attrs=read-write` makes the driver pick the current
  primary. Tested on PostgreSQL 18 with Patroni.

### Replicas

- Set `DEADFIN_REPLICA_URL` on each replica to its own address on the
  private network, for example `http://10.0.50.11:8096` and
  `http://10.0.50.12:8096`. Replica requests are signed with a key that
  Deadfin keeps in the database.
- **Data root: share one between the replicas**
  ([maintainer's answer on #26](https://github.com/deadfin-org/deadfin/issues/26#issuecomment-6744198633)). It holds the
  settings (`config/*.xml`), the library folders (`root/default`),
  collections, playlists, trickplay and local images (`metadata`). A
  separate data root per replica is **not safe**: a replica that reads the
  library list and doesn't find a library's folder on its own disk deletes
  that library from the database, and the other replica then adds it back
  with a new ID. Images and the other files are not copied between
  replicas either.

  We export the data root from the NAS over NFS v4.2, read-write for both
  hosts, at the same path on both. Settings files are written to a
  temporary file and renamed, so a replica never reads half a file.

  - **Stop a replica from starting on an empty data root.** If the NFS
    share isn't mounted, the mount point is an empty local folder, and
    starting on it would delete the libraries as described above. Bind a
    *subfolder* of the share with `create_host_path: false` (example
    below): when the share isn't mounted the subfolder doesn't exist and
    Docker refuses to start the container. Order Docker after the mount
    (`After=` and `Wants=` on the mount unit in a `docker.service`
    drop-in).
  - With root squash on the export, Docker stats the bind source as
    `nobody`. Give the share's top folder execute for others (`0751`) so
    the check can pass; the data root's own folders stay `0700`.
  - Keep `log/` on each host's local disk. Both replicas name their log
    files the same way.

  Two limits in v0.4.0 (see the
  [#26 answer](https://github.com/deadfin-org/deadfin/issues/26#issuecomment-6744198633)):

  - **Settings reach the other replica only when it restarts.** Each
    replica reads the settings files once, at start, and a later save on
    the other replica writes its old copy back. Make settings, library
    and plugin changes on one replica, then restart the other one.
    Library folders and library options are read from disk each time, so
    those agree at once.
  - **Both replicas run the scheduled tasks** (library scan every 12 hours,
    metadata refresh, trickplay, cleanup), and the schedule is shared, so
    they can't be staggered. Start a manual scan on one replica only.
- **Transcode directory:** `cache/transcodes` is inside the shared data
  root, so both replicas already share it, and a remote HLS job moves to
  the other replica without restarting. Optionally, put the cache on its own
  export, as we do: SSD, no snapshots, sync off, owned by the container user
  (65532), mounted on `cache`.

  "Remote" means the job runs on a
  [transcode worker](../transcode-workers.md). With no worker, each
  replica runs FFmpeg itself (the default `local` placement mode), and
  that job stops when its replica stops or its host fails, shared
  directory or not: the client has to start playback again. To keep
  running transcodes through a failover or a rolling update, add at least
  one worker that isn't on either replica host, point it at the floating
  IP's name, and set `DEADFIN_TRANSCODE_WORKER_MODE=workers-first`.

  If you use a separate export, mount it on `cache`, not on `cache/transcodes`: in v0.4.0-rc.1 a mount point there fails at start (`device or resource busy`).

  ```yaml
  services:
    deadfin:
      ports:
        - "10.0.50.11:8096:8096"   # this host's private address; 10.0.50.12 on node B
      environment:
        DEADFIN_REPLICA_URL: http://10.0.50.11:8096   # http://10.0.50.12:8096 on node B
      volumes:
        - type: bind                  # shared data root (NFS), see above
          source: /mnt/deadfin-data/deadfin
          target: /var/lib/deadfin
          bind:
            create_host_path: false   # share not mounted = container doesn't start
        - ./log:/var/lib/deadfin/log  # per host
        - /mnt/deadfin-transcodes:/var/lib/deadfin/cache
        - /mnt/media:/mnt/media:ro
  ```

  Publish 8096 on the private address only. Caddy on the host and the
  other replica reach the container through it; without the mapping the
  proxy returns 502 and requests forwarded between replicas fail.

### Caddy on both nodes

- Run Caddy on the host, not in Compose, so it can bind the floating IP.
  Set `net.ipv4.ip_nonlocal_bind=1` so the node without the floating IP can
  bind it too, and keep Caddy running there.
- Both nodes need the same certificate. Keep Caddy's storage in PostgreSQL
  with a storage module that locks
  (`github.com/dmarcwise/caddy-storage-postgresql`, which uses pgx and
  accepts the same multi-host URL). Then only one node renews at a time.
  This matters with DNS providers whose API replaces the whole zone on
  every write (Namecheap's `setHosts`): two nodes renewing at once could
  overwrite each other's records.
- If your public domain is also your internal DNS zone (split horizon),
  set public `resolvers` in the `tls` block. Otherwise Caddy checks the
  challenge record against your internal DNS and waits forever.
- Pass the database password in `PGPASSWORD`, not in the URL. Caddy logs
  the storage URL at info level when it cleans storage.

```
{
	storage postgresql {
		dsn {env.CADDY_PG_DSN}        # postgres://caddy@10.0.50.62:5432,10.0.50.63:5432/caddy?target_session_attrs=read-write
	}
	auto_https disable_redirects
	default_bind 10.0.60.10
}

media.example.com:8920 {
	tls {
		dns <provider> { ... }
		resolvers 1.1.1.1 9.9.9.9
	}
	reverse_proxy 10.0.50.11:8096 10.0.50.12:8096 {
		lb_policy first               # node A first; node B when A fails its health check
		lb_try_duration 5s            # retry a failed connection on the other replica
		lb_try_interval 250ms
		fail_duration 30s             # passive check: a failed request marks the replica down
		health_uri /System/Info/Public
		health_interval 10s
		flush_interval -1
	}
}
```

- The active health check alone leaves a gap: if node A's replica or host
  fails between two probes, `lb_policy first` keeps choosing it, and Caddy
  returns 502 until the next probe, even though node B is up. Caddy
  doesn't retry or track failures by default. `lb_try_duration` makes it
  try the other replica when a connection fails, and `fail_duration` takes
  a replica out of rotation after a failed request. Moving the floating IP
  doesn't help here, because both nodes' Caddy pick from the same list.

- `KnownProxies`: the private-network addresses of both hosts
  (`10.0.50.11`, `10.0.50.12`). That is the address Deadfin sees for
  Caddy, because Caddy reaches the replicas over the private network. The
  setting is in the shared data root, so set it once and restart the
  other replica.

### keepalived

```
global_defs {
	enable_script_security
	script_user root
}
vrrp_script chk_caddy {
	script "/usr/bin/systemctl is-active --quiet caddy"
	interval 3
	fall 2
	rise 2
}
vrrp_instance DEADFIN_EDGE {
	state BACKUP
	interface bond0.60
	virtual_router_id 60
	priority 150                     # node A; 100 on node B
	advert_int 1
	unicast_src_ip 10.0.60.11        # node B: 10.0.60.12
	unicast_peer {
		10.0.60.12                   # node B: 10.0.60.11
	}
	authentication {
		auth_type PASS
		auth_pass <8 chars>
	}
	virtual_ipaddress {
		10.0.60.10/24 dev bond0.60
	}
	track_script {
		chk_caddy
	}
}
```

Measured on our pair: stopping Caddy on node A moved the floating IP to
node B and served requests again within 4 s, with the same certificate.
The IP returned to node A 7 s after Caddy started there again.
Stopping node A's Deadfin replica (`docker compose stop`, as in a rolling
update) while requesting `/System/Info/Public` through the floating IP
every 0.5 s: 90 of 90 requests answered 200 during the 16 s it was down.
Node A's replica logged `replica drained; its sessions and jobs are free
for the peers` before it stopped.

### Containing the replica (optional)

The client-facing network alone doesn't isolate the Deadfin container: the
host is still on the private network. We filter the container's outbound
traffic with nftables, matched on the Compose bridge
(`driver_opts: com.docker.network.bridge.name: br-deadfin`):

- allow replies, PostgreSQL, the peer replica's port, and anything outside
  the private ranges (metadata providers);
- drop the rest of the private network (NAS, DNS servers, other hosts).

Name resolution: on a user-defined Compose network, Docker's embedded DNS
server forwards lookups to the host's resolvers from the host itself, so
the filter doesn't see them; we checked that metadata names resolve with
the filter loaded. If the container sends DNS straight to a server
instead (a `dns:` entry in Compose, or another runtime), allow UDP and
TCP 53 to that one resolver before the drop rule, or metadata and image
requests fail.

Load it from its own table and systemd unit ordered before
`docker.service`. Rules in `DOCKER-USER` are lost at every reboot, and
`nftables.service` flushes Docker's tables.

### Intel iGPU nodes

On Alder Lake iGPUs (UHD 770), Ubuntu leaves GuC submission off. HEVC
10-bit decode together with a QSV encode then hangs the video engine
(`GPU HANG … hevc_qsv`). Add `i915.enable_guc=3` to the kernel command line.
Gen12 iGPUs decode AV1 but can't encode it.

### Replicas with different GPUs

The encoding settings are in the shared data root, so both replicas use
the same ones. In v0.4.0-rc.1 a replica doesn't check its own encoder
before choosing a codec: with **Allow AV1 encoding** on, a replica whose
GPU can't encode AV1 (UHD 770 next to an Arc A380, for example) starts
`av1_qsv` for a client that prefers AV1, FFmpeg fails
(`Current profile is unsupported`), and the segment request returns 500
(#22). Use GPUs with the same encoders on both replicas, or turn off AV1
encoding.
