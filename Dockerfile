# EdgeTAK container image: a static EdgeTAK binary on distroless (no shell,
# runs as a non-root user), with a pinned MediaMTX for video.
#
#   docker build --build-arg VERSION=v0.3.0 -t edgetak .
#
# Multi-arch builds cross-compile on the build machine, so no emulation is
# needed: docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 .

ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
      go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/edgetak ./cmd/edgetak \
 && mkdir -p /out/data

# MediaMTX, checked against the same pinned checksums as the Linux installer.
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS mediamtx
ARG TARGETARCH TARGETVARIANT
ARG MTX_VERSION=1.21.1
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    case "$TARGETARCH$TARGETVARIANT" in \
      amd64) a=amd64; sum=653abc672a3e693f8d3b2717752492fdcfb8072291ec108d03d3dd857411b0ee ;; \
      arm64) a=arm64; sum=6a3aa635fb60ea9b8d566ec306f0a42ff1b6b52a3942bc2baffbe55880d4c3dd ;; \
      armv7) a=armv7; sum=d032bc572a07a139417db9584a33b91d48c7eb8f27e447645f25c259447e9c07 ;; \
      *) echo "no MediaMTX build for $TARGETARCH$TARGETVARIANT" >&2; exit 1 ;; \
    esac; \
    f="mediamtx_v${MTX_VERSION}_linux_${a}.tar.gz"; \
    curl -fsSL -o "/tmp/$f" "https://github.com/bluenviron/mediamtx/releases/download/v${MTX_VERSION}/$f"; \
    echo "$sum  /tmp/$f" | sha256sum -c -; \
    mkdir -p /out; tar -xzf "/tmp/$f" -C /out mediamtx LICENSE; \
    echo "v${MTX_VERSION}" > /out/mediamtx.version

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/edgetak /usr/local/bin/edgetak
COPY --from=mediamtx /out/ /opt/mediamtx/
COPY --from=build --chown=nonroot:nonroot /out/data /data
COPY LICENSE /usr/share/doc/edgetak/LICENSE
ENV EDGETAK_DATA=/data \
    EDGETAK_CONTAINER=1 \
    EDGETAK_VIDEO=1 \
    EDGETAK_MEDIAMTX_SEED=/opt/mediamtx/mediamtx
VOLUME /data
# CoT TCP/TLS, HTTPS API + console, enrollment, plain HTTP, Let's Encrypt,
# and video (RTSP, RTMP, SRT).
EXPOSE 8087 8089 8443 8446 8080 80 8554 1935 8890/udp
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD ["/usr/local/bin/edgetak", "-healthcheck"]
USER nonroot
ENTRYPOINT ["/usr/local/bin/edgetak"]
