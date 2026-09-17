# Build the manager binary
# For local multi-arch builds, cross-compilation is used for speed.
# CI builds natively per-arch (BUILDPLATFORM == TARGETPLATFORM) which
# preserves CGO and FIPS compliance via the Red Hat go-toolset image.
FROM --platform=$BUILDPLATFORM registry.access.redhat.com/ubi9/go-toolset:1.26.5-1787080706 AS builder

ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /opt/app-root/src

COPY . .
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH make build-operator

FROM --platform=linux/$TARGETARCH registry.access.redhat.com/ubi9/ubi-minimal:latest@sha256:8eb2830d0936237fc13a1f2f7e45aecf90d69043380ad167fad0343632937f41
WORKDIR /
COPY --from=builder /opt/app-root/src/bin/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
