

ARG SOURCE_DATE_EPOCH
# deadfin-web, the default web client. The image index is pinned by digest;
# Renovate opens a bump pull request for each deadfin-web release. Release
# order: tag deadfin-web, merge the bump, then tag deadfin (docs/releases.md).
ARG DEADFIN_WEB_IMAGE=ghcr.io/deadfin-org/deadfin-web:v0.3.0-rc.9@sha256:17ef7745c781959d91561cc883d00d6b0c1527bf5372787c7fdc3478f533c840
FROM --platform=$BUILDPLATFORM golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
RUN dpkg --add-architecture "$TARGETARCH" && apt-get update && case "$TARGETARCH" in amd64) compiler=/usr/bin/x86_64-linux-gnu-gcc; pkgconfig=/usr/lib/x86_64-linux-gnu/pkgconfig; packages='build-essential pkg-config gcc-x86-64-linux-gnu libc6-dev-amd64-cross libvips-dev:amd64' ;; arm64) compiler=/usr/bin/aarch64-linux-gnu-gcc; pkgconfig=/usr/lib/aarch64-linux-gnu/pkgconfig; packages='build-essential pkg-config gcc-aarch64-linux-gnu libc6-dev-arm64-cross libvips-dev:arm64' ;; *) echo "unsupported target architecture: $TARGETARCH" >&2; exit 1 ;; esac && apt-get install --yes --no-install-recommends $packages && ln -s "$compiler" /usr/local/bin/deadfin-target-gcc && ln -s "$pkgconfig" /usr/local/lib/deadfin-target-pkgconfig && rm -rf /var/lib/apt/lists/*
ENV CC=/usr/local/bin/deadfin-target-gcc \
    PKG_CONFIG_LIBDIR=/usr/local/lib/deadfin-target-pkgconfig:/usr/share/pkgconfig

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . ./
# Build identity. The context has no .git
# (.dockerignore), so the generator always runs in explicit mode from the
# build-args; all four are required and validated before any file is staged.
# GOOS/GOARCH are unset here so the generator runs natively on BUILDPLATFORM.
ARG REVISION VERSION CHANNEL DIRTY SOURCE_DATE_EPOCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /buildinfo && CGO_ENABLED=0 go run ./tools/buildinfo-gen -o /buildinfo

FROM build AS image-test-build
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go test -c -trimpath -o /out/images.test ./internal/images

FROM build AS app-build
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -buildvcs=false -overlay=/buildinfo/overlay.json -ldflags="-s -w -buildid=" -o /out/deadfin ./cmd/deadfin && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o /out/perfcompare ./tools/perfcompare

FROM build AS replay-tools-build
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o /out/finalreplaybootstrap ./tools/finalreplaybootstrap && \
    CGO_ENABLED=1 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o /out/goldenrec-seed-user ./tools/goldenrec-seed-user

# Jellyfin FFmpeg: tonemapx for software HDR tone mapping, and
# the Intel iHD VAAPI driver, oneVPL QSV runtime, and OpenCL filters for
# hardware transcoding. Each release asset is pinned by SHA-256
# (docs/06-risks.md: pinned version, clear startup error).
FROM --platform=$BUILDPLATFORM golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS jellyfin-ffmpeg
ARG TARGETARCH
RUN case "$TARGETARCH" in amd64) digest=33b6defb6fcd26c366e1fe062aa52af8b0157a4003ce67a8b3ae6b54245ebb3d ;; arm64) digest=062efa04b8f0de43fd4951d18095eaf16b5690cecf5fc19243cf52d781dba362 ;; *) echo "unsupported target architecture: $TARGETARCH" >&2; exit 1 ;; esac && mkdir -p /out && curl --fail --silent --show-error --location --output /out/jellyfin-ffmpeg.deb "https://github.com/jellyfin/jellyfin-ffmpeg/releases/download/v8.1.1-1/jellyfin-ffmpeg8_8.1.1-1-bookworm_${TARGETARCH}.deb" && echo "$digest  /out/jellyfin-ffmpeg.deb" | sha256sum --check --strict -

# SkiaSharp native library from the official SkiaSharp.NativeAssets.Linux
# NuGet package, the version the pinned Jellyfin revision uses. skia-fetch
# verifies the SHA-256 of the package and of the extracted library. Only the
# tool's own files are copied, so source edits do not invalidate this layer.
FROM --platform=$BUILDPLATFORM golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS skiasharp
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY tools/skia-fetch/main.go tools/skia-fetch/
RUN --mount=type=cache,target=/root/.cache/deadfin-skiasharp \
    CGO_ENABLED=0 go run ./tools/skia-fetch -arch "$TARGETARCH" -cache /root/.cache/deadfin-skiasharp -o /out/libSkiaSharp.so

# The deadfin-web release image holds the static bundle at /deadfin-web. The
# files do not depend on the architecture.
FROM --platform=$BUILDPLATFORM ${DEADFIN_WEB_IMAGE} AS deadfin-web

FROM --platform=$TARGETPLATFORM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS runtime
ARG REVISION
ARG VERSION
LABEL org.opencontainers.image.source="https://github.com/deadfin-org/deadfin" \
      org.opencontainers.image.licenses="GPL-2.0-only" \
      org.opencontainers.image.revision="$REVISION" \
      org.opencontainers.image.version="$VERSION"
ARG TARGETARCH
COPY --from=jellyfin-ffmpeg /out/jellyfin-ffmpeg.deb /tmp/jellyfin-ffmpeg.deb
# intel-opencl-icd provides the OpenCL runtime for the QSV/VAAPI OpenCL tone
# mapping path on Intel GPUs. jellyfin-ffmpeg only suggests it.
RUN case "$TARGETARCH" in amd64) opencl=intel-opencl-icd ;; *) opencl= ;; esac && apt-get update && apt-get install --yes --no-install-recommends ca-certificates libvips42 libvips-tools $opencl /tmp/jellyfin-ffmpeg.deb && rm -rf /var/lib/apt/lists/* /tmp/jellyfin-ffmpeg.deb
# Keep the ffmpeg and ffprobe command names the server already uses.
ENV PATH=/usr/lib/jellyfin-ffmpeg:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
RUN groupadd --gid 65532 deadfin && useradd --uid 65532 --gid 65532 --create-home --home-dir /var/lib/deadfin --shell /usr/sbin/nologin deadfin && mkdir -p /var/lib/deadfin && chown -R 65532:65532 /var/lib/deadfin
# The image default sits in the lowest-precedence variable, so a deployment
# that sets either DEADFIN_DATA_DIR or JELLYFIN_DATA_DIR overrides it.
ENV JELLYFIN_DATA_DIR=/var/lib/deadfin

COPY --from=skiasharp /out/libSkiaSharp.so /usr/local/lib/deadfin/libSkiaSharp.so
COPY third_party/skia-sharp/3.119.4/ /usr/share/licenses/skia-sharp/3.119.4/
# Deadfin's license and provenance, and the third-party NOTICE with the source offer.
COPY LICENSE PROVENANCE.md third_party/deadfin-image/NOTICE /usr/share/doc/deadfin/
# Deadfin serves this directory at /web/ when <data root>/web does not exist
# (cmd/deadfin/webclient.go). Its licenses/ folder holds the notices of the
# bundled fonts; deadfin-web itself is GPL-2.0-only, like Deadfin.
COPY --from=deadfin-web /deadfin-web/ /usr/share/deadfin/web/
COPY --from=deadfin-web /deadfin-web/licenses/ /usr/share/licenses/deadfin-web/
FROM runtime AS image-test
COPY --from=image-test-build --chown=65532:65532 /out/images.test /opt/images/images.test
COPY --chown=65532:65532 internal/images/testdata/ /opt/images/testdata/
WORKDIR /opt/images
USER 65532:65532
ENTRYPOINT ["./images.test"]

FROM runtime AS replay-tools
COPY --from=replay-tools-build --chown=65532:65532 /out/finalreplaybootstrap /usr/local/bin/finalreplaybootstrap
COPY --from=replay-tools-build --chown=65532:65532 /out/goldenrec-seed-user /usr/local/bin/goldenrec-seed-user
USER 65532:65532
WORKDIR /var/lib/deadfin

FROM runtime
COPY --from=app-build --chown=65532:65532 /out/deadfin /usr/local/bin/deadfin
# The Jellyfin comparison tool rides in the release image so that the
# nightly comparison CronJob runs the same revision as the server.
COPY --from=app-build --chown=65532:65532 /out/perfcompare /usr/local/bin/perfcompare
USER 65532:65532
EXPOSE 8096
ENTRYPOINT ["/usr/local/bin/deadfin"]
