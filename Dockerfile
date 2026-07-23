# unifi-emu — static, scratch-based image of the device emulator CLI.
#
# The CA bundle comes along from the build stage. It is inert while TLS
# verification is off (the adopt helper skips verification against
# self-signed controller certs); kept so enabling verification later is
# a one-line change. Scratch has no shell — flags/env only:
#   docker run --rm unifi-emu:dev -inform http://CTRL:8080/inform -mac 00:27:22:e0:00:31
# Build on the native BUILDPLATFORM and cross-compile to TARGETOS/TARGETARCH
# (CGO is off, so no emulation needed — buildx multi-arch stays fast). VERSION
# stamps the CLI so `docker run ... -V` reports the release version.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w -X main.buildVersion=$VERSION" -o /unifi-emu ./cmd/unifi-emu

FROM scratch
COPY --from=build /etc/ssl/certs /etc/ssl/certs
COPY --from=build /unifi-emu /unifi-emu
ENTRYPOINT ["/unifi-emu"]
