# Root-level Dockerfile for Portainer Image-build URL deployments which
# can't reliably specify a non-default Dockerfile path. Same build logic
# as docker/Dockerfile but stripped of BuildKit-only directives so it
# works on legacy builders too.

ARG PYTHON_VERSION="3.11"
ARG GO_VERSION="1.25"

# 1. Build go2rtc binary
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH

ENV GOOS=${TARGETOS}
ENV GOARCH=${TARGETARCH}

WORKDIR /build

RUN apk add git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -trimpath


# 2. Final image
FROM python:${PYTHON_VERSION}-alpine AS base

RUN apk add --no-cache tini ffmpeg ffplay bash curl jq alsa-plugins-pulse font-droid

ARG TARGETARCH
RUN if [ "${TARGETARCH}" = "amd64" ]; then apk add --no-cache libva-intel-driver intel-media-driver; fi

COPY --from=build /build/go2rtc /usr/local/bin/

ENTRYPOINT ["/sbin/tini", "--"]
VOLUME /config
WORKDIR /config

CMD ["go2rtc", "-config", "/config/go2rtc.yaml"]
