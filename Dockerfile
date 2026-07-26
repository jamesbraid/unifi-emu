# unifi-emu — static, scratch-based image of the device emulator CLI.
#
# The image bakes the 5-device test fleet as its default, so a bare run boots
# it; any explicit source overrides it (Scratch has no shell — flags/env only):
#   docker run --rm unifi-emu:dev -inform http://CTRL:8080/inform        # baked fleet
#   docker run --rm unifi-emu:dev -inform http://CTRL:8080/inform \
#     -e SIM_MODELS=U7PRO,USM8P:2,UGW3                                   # pick models
#   docker run --rm unifi-emu:dev -inform http://CTRL:8080/inform -mac 00:27:22:e0:00:31  # one device
#
# Adding SIM_ADOPT makes the container adopt what it informs, reaching the
# controller's API port as well as its inform port:
#   docker run --rm -e SIM_ADOPT=1 -e SIM_ADOPT_URL=https://CTRL:8443 \
#     -e SIM_ADOPT_USERNAME=admin -e SIM_ADOPT_PASSWORD=admin \
#     unifi-emu:dev -inform http://CTRL:8080/inform
#
# The CA bundle comes along from the build stage. It is inert while TLS
# verification is off (adoption skips verification against self-signed
# controller certs); kept so enabling verification later is a one-line
# change.
# Build on the native BUILDPLATFORM and cross-compile to TARGETOS/TARGETARCH
# (CGO is off, so no emulation needed — buildx multi-arch stays fast). VERSION
# stamps the CLI so `docker run ... -V` reports the release version.
ARG BUILDPLATFORM
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
# Bake the default test fleet. SIM_DEFAULT_DEVICES is the lowest-priority
# fleet source: consulted only when the run names no source of its own, so a
# bare `docker run` boots this fleet while -e SIM_MODELS/-e SIM_DEVICES or the
# single-device flags override it without a "ambiguous sources" collision.
COPY scripts/devices.fleet.json /devices.json
ENV SIM_DEFAULT_DEVICES=/devices.json
ENTRYPOINT ["/unifi-emu"]
