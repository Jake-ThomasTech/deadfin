# verify-env — Dockerized verify gates

## Purpose

Run Jellyport `make verify-*` gates in a container. The host needs only
Docker Engine with the compose and buildx plugins. The host does not need
Go, golangci-lint, the .NET SDK, PostgreSQL, ffmpeg, libvips, or
libSkiaSharp.

## Requirements

- Docker Engine with the compose and buildx plugins. Docker Desktop is supported
  on Darwin/arm64; Docker Engine is supported on Linux/amd64.
- The invoking user can use `/var/run/docker.sock`.
- The Jellyport and Deadfin repositories are checked out separately.
- The selected Jellyport checkout must contain a Deadfin checkout at `deadfin/`.

## Usage

```
tools/verify-env/run.sh                # runs `make verify-p6`
tools/verify-env/run.sh make lint      # any other command in the gate
```

Set `VERIFY_ENV_REPO` to run against a different Jellyport checkout.

Generate the committed synthetic media projection with:

```
tools/verify-env/generate-fixtures.sh
```

This separate service always uses `linux/amd64`, including on Darwin/arm64.
It pins Debian package indexes at snapshot `20260728T000000Z` and the ffmpeg
package at `7:5.1.9-0+deb12u1`. It accepts the result only when every digest
matches `source-manifest.sha256`. On Apple Silicon, the run will fail at the
fixture-publisher step: the candidate differs from `source-manifest.sha256`
in exactly the 18 MPEG-4 `.mkv` entries, and that is expected rather than a
bug. Fixture correctness is judged on the native x86_64 CI runner and only
there. Use `--attest` only
after an intentional canonical image change. To re-attest, dispatch the
`goldenrec-gate` workflow with `attest=true`, which publishes
`source-manifest.sha256` as a downloadable artifact and does not commit it.

The generator forces a fixed scalar single-threaded video profile for the 18
mpeg4 MKV files. Every mpeg4 MKV command passes `-cpuflags 0 -threads 1`
before its inputs. This removes the SIMD CPU dispatch and the encoder thread
pools from the video encoders. The `-cpuflags 0 -threads 1` flags do not make
the 18 MPEG-4 files host-independent. The encoder still selects scalar code
paths that differ between host architectures, which is why the 18 MPEG-4
`.mkv` entries diverge on Apple Silicon. The HLG sample is a committed
artifact. The
generator copies it from `tools/goldenrec/fixtures/attested/hlg-sample.mkv`
after it verifies the sha256 against a pinned digest in `gen.sh`. macOS/arm64
generation is a verification path, not an attestation authority. The native
x86_64 CI runner remains the sole attestation authority for
`source-manifest.sha256`.

## Design

- The gate image pins the same `golang:1.26.10-bookworm` digest as the
  Deadfin Dockerfile build stage. It adds ffmpeg, libvips-dev, python3,
  jq, golangci-lint v2.12.2, the exact .NET SDK 10.0.302, and the Docker CLI
  with buildx. It downloads exactly one of these immutable SDK artifacts and
  verifies its recorded SHA-512 before extraction:
  - `dotnet-sdk-10.0.302-linux-x64.tar.gz` —
    `10069bec8783596484a610332f090d562802a41b9b40e3327a5a5688b572e10c296ae300f940d40461f23c157ed1b0843c2f8e6b3f20d8d8d9d83432d8143bac`
  - `dotnet-sdk-10.0.302-linux-arm64.tar.gz` —
    `9e409c14e00686d661c78fa4dd9ad0e4dcf695c328bd5ff777d05b4a9c34b42cf89b12573b92e9fb2f565dbe12016b4835f77c7d9a42b55a7494df21634cd5d6`
  The entrypoint prints `verify-env: .NET SDK: 10.0.302` and rejects a
  different resolved SDK version before the requested gate starts.
- The image follows the Docker daemon's native Linux architecture; no
  `DOCKER_DEFAULT_PLATFORM` setting is required. The entrypoint normalizes
  `x86_64`/`amd64` to `linux-amd64` and `aarch64`/`arm64` to `linux-arm64`,
  then installs only the matching pinned `libSkiaSharp.so`. It fails rather
  than selecting a different architecture.
- The entrypoint runs `tools/skia-fetch` from the mounted Deadfin checkout. It
  installs `libSkiaSharp.so` to `/usr/local/lib/deadfin/`, the fixed `dlopen`
  path in `internal/images/skia_webp_linux_cgo.go`. The tool takes the library
  from the SkiaSharp.NativeAssets.Linux NuGet package and verifies the package
  and the library by SHA-256. The verified library is cached in the `/cache`
  volume, so only the first run downloads it. The library version tracks the
  repository, not the image.
- A `postgres` sidecar provides `DEADFIN_TEST_POSTGRES_DSN` on
  `127.0.0.1:5433`. Its data directory is a tmpfs. The store tests create
  and drop their own schemas, so no database state is kept.
- The gate container uses the host network namespace and the host Docker
  socket. `make image-verify` builds on the host daemon, publishes the
  verifier container port on the host loopback, and curls `127.0.0.1`.
  Host networking makes that address correct from inside the gate.
- The wrapper prefers `stat -c %g` on Linux and `stat -f %g` on Darwin to
  read the host Docker socket group. If a host resolves the other stat dialect
  first, it retries that dialect. After building the gate, it reads the mounted
  socket's numeric GID in an isolated metadata-only container. This handles
  Docker Desktop group remapping before it starts the final non-root gate with
  that effective group. The gate retains the invoking user's UID and GID, so
  files it writes into the mounted repository (for example NuGet `obj/`
  directories) belong to that user.
- The fixture-generator service is separate from the native-architecture gate.
  It uses the same digest-pinned Go base, a fixed Debian snapshot, and
  `platform: linux/amd64`. Its entrypoint verifies Linux/x86_64 and the exact
  ffmpeg package before it runs. The publisher verifies a root-owned image
  attestation, Debian 12 snapshot sources, platform, and package again. Each
  invocation has a unique Compose project and a fixed `/work` mount. This avoids
  cross-checkout resource collisions and Darwin `/tmp` path aliases. Before
  generation, the wrapper probes the mount through an image-resident `/bin/sh`.
  If the container cannot read the entrypoint and `source-manifest.sha256`
  under `/work`, the wrapper prints a bind-mount error and exits before
  publication.
- The publisher accepts only an empty private sibling directory. It creates the
  candidate with a fixed ffmpeg path and a clean environment. It verifies the
  complete 57-entry manifest and the 30 media digests before it takes the short
  repository publication lock. It then promotes the candidate and updates an
  attested source manifest through a same-directory temporary file. A generation
  failure or rejected candidate leaves accepted output unchanged. Direct host
  generator and publisher runs cannot replace that output.

## What stays on the host

- The gate image and the postgres image.
- The `jellyport-verify-env_verify-cache` named volume with the Go,
  NuGet, and lint caches for local runs. Keep it; it makes local reruns fast.
  CI uses a private volume per run and removes it at completion; its cache
  starts cold so concurrent runs cannot delete one another’s resources.
- Deadfin images that `image-verify` builds. Host-run gates already
  produce these today.

Nothing is installed on the host itself.

## Cleanup

```
docker compose -f tools/verify-env/compose.yaml down --rmi local
docker volume rm jellyport-verify-env_verify-cache
```

Do not use `docker system prune --volumes` in this repository's flows.

## Focused regression

Run the Docker-free shell regressions before a gate change:

```
sh tools/verify-env/portability_test.sh
python3 -m unittest tools.goldenrec.tests.test_fixture_determinism
```

They mock both host `stat` variants, reject an unsupported architecture, and
exercise publication across isolated repositories.

### Parallel CI ownership

Source-attestation sets a unique COMPOSE_PROJECT_NAME per run and attempt.
It sets VERIFY_ENV_POSTGRES_PORT=0, then run.sh reads the allocated loopback
port and passes its DSN into the host-network gate. Local runs still default
to 5433. The final CI cleanup removes only that run’s containers and cache
volume. Do not remove a shared project to recover another job’s collision.
