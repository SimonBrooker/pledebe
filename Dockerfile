# Static build — no cgo, so this cross-compiles to arm64 without QEMU.
# That matters: most pledebe users are on NAS hardware, and emulated builds are
# slow enough to dominate CI time.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src
COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o /out/pledebe ./cmd/pledebe

# alpine rather than scratch: users on a NAS occasionally need a shell to work
# out why a mount is not visible, and the size difference is trivial next to the
# ~218MB Plex SQLite directory that lives in the data volume.
FROM alpine:3.20

RUN adduser -D -u 1000 pledebe
COPY --from=build /out/pledebe /usr/local/bin/pledebe

# Runs unprivileged. Monitor mode only ever needs read access, and the config
# volume should be mounted :ro.
USER pledebe

ENTRYPOINT ["/usr/local/bin/pledebe"]
