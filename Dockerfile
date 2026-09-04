# syntax=docker/dockerfile:1

# Multi-arch, CGO-free container for dolmen.
# Build: docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/lsm/dolmen:vX.Y.Z --build-arg VERSION=vX.Y.Z --push .

ARG VERSION=devel
ARG TARGETOS=linux
ARG TARGETARCH=amd64

FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine AS build
ARG VERSION
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags "-X github.com/lsm/dolmen/internal/version.Version=${VERSION}" \
    -o /out/dolmen .

# gcr.io/distroless/static is a multi-arch, root-user image with CA certificates
# and no shell. Pinned to the OCI index digest for reproducibility.
FROM --platform=$TARGETPLATFORM gcr.io/distroless/static@sha256:f2ea2709ac8db56323cbd7d014277f32cb572d9ea124b0076f7aafe5980678fe

WORKDIR /data
COPY --from=build /out/dolmen /dolmen

EXPOSE 8790
VOLUME ["/data"]

ENTRYPOINT ["/dolmen"]
CMD ["-addr", "0.0.0.0:8790", "-data", "/data"]
