# Gate environment image for Jellyport `make verify-*` targets.
# Pins the same golang base digest as the Deadfin Dockerfile build stage.
# See README.md in this directory for the design and usage.
FROM golang:1.26.10-bookworm@sha256:23ce71337b726608afb1bc00986e630a05034a87a87ea1fc2cd1ae5cf33387eb

ARG GOLANGCI_LINT_VERSION=v2.12.2
ARG TARGETARCH
ARG DOTNET_SDK_VERSION=10.0.302
ARG DOTNET_SDK_SHA512_AMD64=10069bec8783596484a610332f090d562802a41b9b40e3327a5a5688b572e10c296ae300f940d40461f23c157ed1b0843c2f8e6b3f20d8d8d9d83432d8143bac
ARG DOTNET_SDK_SHA512_ARM64=9e409c14e00686d661c78fa4dd9ad0e4dcf695c328bd5ff777d05b4a9c34b42cf89b12573b92e9fb2f565dbe12016b4835f77c7d9a42b55a7494df21634cd5d6

# ffmpeg + libvips for the Deadfin test suites; python3 + jq for the
# Jellyport tooling; libicu72 for the .NET SDK.
RUN apt-get update && apt-get install --yes --no-install-recommends \
        ffmpeg libvips-dev libvips-tools python3 jq libicu72 \
    && rm -rf /var/lib/apt/lists/*

# Docker CLI + buildx for `make image-verify`. Builds run on the host
# daemon through the mounted /var/run/docker.sock.
RUN install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian bookworm stable" \
        > /etc/apt/sources.list.d/docker.list \
    && apt-get update && apt-get install --yes --no-install-recommends \
        docker-ce-cli docker-buildx-plugin \
    && rm -rf /var/lib/apt/lists/*

# go install verifies the module against the Go checksum database.
RUN GOBIN=/usr/local/bin go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}"

# The fixed SDK artifact and its SHA-512 keep generated assembly provenance
# reproducible. TARGETARCH comes from the Docker build platform.
RUN case "$TARGETARCH" in \
        amd64) dotnet_arch=x64; dotnet_sha512="$DOTNET_SDK_SHA512_AMD64" ;; \
        arm64) dotnet_arch=arm64; dotnet_sha512="$DOTNET_SDK_SHA512_ARM64" ;; \
        *) echo "unsupported .NET SDK architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac \
    && dotnet_archive="dotnet-sdk-${DOTNET_SDK_VERSION}-linux-${dotnet_arch}.tar.gz" \
    && curl -fsSL "https://builds.dotnet.microsoft.com/dotnet/Sdk/${DOTNET_SDK_VERSION}/${dotnet_archive}" -o "/tmp/${dotnet_archive}" \
    && echo "${dotnet_sha512}  /tmp/${dotnet_archive}" | sha512sum -c - \
    && mkdir -p /usr/share/dotnet \
    && tar -xzf "/tmp/${dotnet_archive}" -C /usr/share/dotnet \
    && ln -s /usr/share/dotnet/dotnet /usr/local/bin/dotnet \
    && rm "/tmp/${dotnet_archive}"

# The gate runs as the invoking host user (compose sets user:). /cache is a
# named volume for toolchain caches; /usr/local/lib/deadfin receives the
# pinned libSkiaSharp.so from tools/skia-fetch at container start (fixed
# dlopen path in internal/images/skia_webp_linux_cgo.go).
RUN mkdir -p /cache /cache/home /usr/local/lib/deadfin \
    && chmod 1777 /cache /cache/home \
    && chmod 0777 /usr/local/lib/deadfin

ENV HOME=/cache/home \
    GOPATH=/cache/go \
    GOCACHE=/cache/go-build \
    GOLANGCI_LINT_CACHE=/cache/golangci-lint \
    DOTNET_CLI_HOME=/cache/dotnet \
    NUGET_PACKAGES=/cache/nuget \
    XDG_CACHE_HOME=/cache/xdg \
    DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1 \
    VERIFY_ENV_DOTNET_SDK_VERSION=${DOTNET_SDK_VERSION}

COPY entrypoint.sh lib.sh /usr/local/bin/

WORKDIR /work
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["make", "verify-p6"]
