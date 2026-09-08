# syntax=docker/dockerfile:1
#
# Builds a kind node image with the time-dilation-patched gVisor runtime baked
# in. Consumed by MytraAI/mytra-helm-charts as its local time-dilated control
# plane node image (KIND_NODE_IMAGE / env.timeDilation.nodeImage).
#
# Published multi-arch (linux/amd64 + linux/arm64) by
# .github/workflows/build-time-dilation-image.yml to
# us-west1-docker.pkg.dev/mytra-artifacts/mytra-docker-internal/mytra-kind-gvisor.
#
# Built FROM the repository context (this synthetic, Go-only `time-dilation`
# branch), so `go build` compiles runsc + the containerd shim with no Bazel.
ARG GO_VERSION=1.26
ARG KIND_NODE_VERSION=v1.35.0

FROM golang:${GO_VERSION} AS build
# TARGETARCH is provided by BuildKit (native per-arch runners or buildx alike),
# so the binaries always match the node image's architecture.
ARG TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/runsc ./runsc \
 && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/containerd-shim-runsc-v1 ./shim

FROM kindest/node:${KIND_NODE_VERSION}
COPY --from=build /out/runsc /out/containerd-shim-runsc-v1 /usr/local/bin/
COPY runsc.toml /etc/containerd/runsc.toml
RUN chmod 0755 /usr/local/bin/runsc /usr/local/bin/containerd-shim-runsc-v1
