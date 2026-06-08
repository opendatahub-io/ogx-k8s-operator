# Build the ogx-module manager binary.
ARG GOLANG_VERSION=1.25

FROM registry.access.redhat.com/ubi9/go-toolset:${GOLANG_VERSION} AS builder
ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /workspace

# Copy module dependencies first for better layer reuse.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy only the module-local inputs needed for the binary and staged manifests.
COPY cmd/ cmd/
COPY pkg/ pkg/
COPY config/manifests/ config/manifests/

# Build the manager binary.
RUN TARGET_ARCH="${TARGETARCH:-$(go env GOARCH)}" && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGET_ARCH}" \
    go build -o /workspace/bin/manager ./cmd/ogx-module

# OpenShift may run the container with an arbitrary UID, so make manifests readable.
RUN chmod -R a+rX config/manifests/

FROM registry.access.redhat.com/ubi9/ubi-micro:latest
WORKDIR /

COPY --from=builder /workspace/bin/manager .
COPY --from=builder /workspace/config/manifests/ /manifests/

USER 65532:65532

ENTRYPOINT ["/manager"]
