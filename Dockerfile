# Build the manager binary
# Override BASE_IMAGE to build from another registry, e.g. docker.io/library/golang:1.26
ARG BASE_IMAGE=golang:1.26
# Run the builder natively on the build host and cross-compile with GOARCH.
# On an Apple Silicon Mac that avoids running the Go toolchain under amd64 emulation.
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# TARGETARCH comes from --platform (the Makefile defaults it to linux/amd64, which is the cluster's arch).
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
