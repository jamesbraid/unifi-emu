# Fixture runtime image: a public test double for the opaque OCI runtimes
# the herder starts for some device models. The herder's tests build it, so
# it is never pushed and never tagged after anything it stands in for.
#
# Build from the repository root, which is where the module's go.mod lives:
#   docker build -f internal/herder/testdata/runtime-fixture/Dockerfile -t <local-tag> .
#
# Only go.mod/go.sum and this directory are copied: the fixture imports the
# standard library alone, so nothing else in the tree can break its build or
# bust its layer cache.
#
# The build stage stays on the host's native platform and cross-compiles
# (CGO is off), so an arm64 host builds the amd64 binary without emulation.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/herder/testdata/runtime-fixture ./internal/herder/testdata/runtime-fixture
# testdata/ is invisible to ./... , so the package path must be explicit.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /fixture ./internal/herder/testdata/runtime-fixture

# Pinned to the platform the binary was cross-compiled for, so the image
# metadata agrees with its contents whatever host builds it.
FROM --platform=linux/amd64 scratch
COPY --from=build /fixture /fixture
# Scratch has no shell, so the binary probes itself: healthy only while the
# process that validated its input and started informing is alive. No EXPOSE
# — the fixture only dials out.
HEALTHCHECK --interval=2s --timeout=2s --start-period=2s --retries=3 \
    CMD ["/fixture", "-healthcheck"]
ENTRYPOINT ["/fixture"]
