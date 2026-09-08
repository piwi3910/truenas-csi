# Build context is the REPOSITORY ROOT, not operator/:
#
#   docker build -f operator/Dockerfile -t ghcr.io/piwi3910/truenas-csi-operator:0.1.0 .
#
# The operator renders deploy/helm/truenas-csi at runtime, so the chart has to
# be in the image. Copying it here — rather than embedding a duplicate under
# operator/ — is what keeps a single copy of the manifests in the repository:
# `helm install` and the operator read the same files.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# The operator is its own module, so only its manifest is needed to warm the
# module cache. GOWORK=off keeps the workspace out of the container build: the
# image must build from the module's own go.mod, exactly as `go install` would.
ENV GOWORK=off
COPY operator/go.mod operator/go.sum ./operator/
RUN cd operator && go mod download

COPY operator/ ./operator/
RUN cd operator && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/manager ./cmd

FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/manager /manager
COPY deploy/helm/truenas-csi /chart/truenas-csi

USER 65532:65532
ENTRYPOINT ["/manager"]
