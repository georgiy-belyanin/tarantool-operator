# Build the manager binary.
# The Go version must satisfy the `go` directive in go.mod (currently 1.25).
FROM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH
# Optional Go build tags, e.g. GO_BUILD_TAGS=vault to compile in the Vault
# password source. Empty by default — the stock image carries no optional module.
ARG GO_BUILD_TAGS=""

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY . .

# Build for the target platform (defaults to linux/amd64). Multi-arch builds set
# TARGETOS/TARGETARCH via `docker buildx build --platform=...`.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -a -tags "${GO_BUILD_TAGS}" -o manager main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
