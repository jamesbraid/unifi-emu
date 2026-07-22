# unifi-emu — static, scratch-based image of the device emulator CLI.
#
# The binary talks TLS to controllers (https inform URLs), so the CA
# bundle comes along from the build stage; scratch has no shell, the
# entrypoint is driven purely by flags/env:
#   docker run --rm unifi-emu:dev -inform http://CTRL:8080/inform -mac 00:27:22:e0:00:31
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /unifi-emu ./cmd/unifi-emu

FROM scratch
COPY --from=build /etc/ssl/certs /etc/ssl/certs
COPY --from=build /unifi-emu /unifi-emu
ENTRYPOINT ["/unifi-emu"]
