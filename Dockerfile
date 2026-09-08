# Build stage. Cross-compiles for the requested target so a native runner can
# still produce either architecture if it has to.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags "-s -w -X github.com/piwi3910/truenas-csi/internal/driver.Version=${VERSION}" \
    -o /out/truenas-csi ./cmd/truenas-csi

# Runtime stage. The node plugin drives the HOST's iscsiadm, mount.nfs and
# filesystem tools through the host mount namespace — deliberately, so it
# shares iscsid state with whatever else uses the node's iSCSI stack — so the
# image itself needs no storage tooling and stays distroless.
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="truenas-csi" \
      org.opencontainers.image.description="CSI driver for TrueNAS SCALE (NFS and iSCSI)" \
      org.opencontainers.image.source="https://github.com/piwi3910/truenas-csi" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /out/truenas-csi /truenas-csi

# The node DaemonSet overrides this with privileged: true; the controller runs
# as this unprivileged user.
USER 65532:65532

ENTRYPOINT ["/truenas-csi"]
