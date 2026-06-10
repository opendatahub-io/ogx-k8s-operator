# Build the ogx-module manager binary.
ARG GOLANG_VERSION=1.25

# Use BUILDPLATFORM so the builder itself always runs natively, while the Go
# binary is cross-compiled for TARGETPLATFORM.
FROM --platform=$BUILDPLATFORM registry.access.redhat.com/ubi9/go-toolset:${GOLANG_VERSION} AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG BUILDPLATFORM
ARG TARGETPLATFORM

USER root
WORKDIR /workspace/ogx-module

# Copy module dependencies first for better layer reuse.
COPY ogx-module/go.mod go.mod
COPY ogx-module/go.sum go.sum
RUN go mod download

# Copy only the module-local code needed for the manager binary.
COPY ogx-module/cmd/ cmd/
COPY ogx-module/internal/ internal/
COPY ogx-module/pkg/ pkg/

# Build the manager binary.
RUN mkdir -p /workspace/bin && \
    echo "Building ogx-module for TARGETPLATFORM=${TARGETPLATFORM} on BUILDPLATFORM=${BUILDPLATFORM}" && \
    TARGET_ARCH="${TARGETARCH:-$(go env GOARCH)}" && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGET_ARCH}" \
    go build -o /workspace/bin/manager ./cmd/ogx-module

# Copy the module hack scripts and the root OGX operator manifests source, then
# stage the root operator manifests into config/manifests inside the image build.
# This mirrors the older ODH-style pattern where manifests are collected locally
# and then baked into the controller image.
COPY ogx-module/hack/ hack/
COPY config/ ../config/
RUN USE_LOCAL=true bash hack/get_manifests.sh

# OpenShift may run the container with an arbitrary UID, so make the staged
# manifests readable in the final image.
RUN chmod -R a+rX config/manifests/

FROM --platform=$TARGETPLATFORM registry.access.redhat.com/ubi9/ubi-micro:latest
WORKDIR /

COPY --from=builder /workspace/bin/manager .
COPY --from=builder /workspace/ogx-module/config/manifests/ /manifests/

USER 65532:65532

ENTRYPOINT ["/manager"]
