# Root-level Dockerfile for legacy-builder deploys (Ansible, Portainer
# Images UI) — same surface as docker/Dockerfile but stripped of all
# BuildKit-only directives:
#   - no `# syntax=...` line
#   - no `--platform=$BUILDPLATFORM` (BuildKit-only var, empty on legacy)
#   - no $TARGETARCH conditionals (legacy build is always native arch)
#   - no `--mount=type=cache` (BuildKit-only)
#
# Native-arch build only. If you need multi-arch images, use buildx
# against docker/Dockerfile instead.

ARG PYTHON_VERSION="3.11"
ARG GO_VERSION="1.25"

# 1. Build go2rtc binary
FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /build

RUN apk add git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -trimpath


# 2. Final image
FROM python:${PYTHON_VERSION}-alpine AS base

# tini for signal handling, ffmpeg/ffplay for stream ops, common shell tools
RUN apk add --no-cache tini ffmpeg ffplay bash curl jq alsa-plugins-pulse font-droid

# Intel VAAPI for hardware acceleration (amd64 hosts only — runtime uname check
# avoids breaking the build on non-amd64 hosts; amount installed is small enough
# to not bother conditionalising at build time).
RUN if [ "$(uname -m)" = "x86_64" ]; then apk add --no-cache libva-intel-driver intel-media-driver; fi

COPY --from=build /build/go2rtc /usr/local/bin/

ENTRYPOINT ["/sbin/tini", "--"]
VOLUME /config
WORKDIR /config

CMD ["go2rtc", "-config", "/config/go2rtc.yaml"]
