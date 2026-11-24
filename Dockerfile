# syntax=docker/dockerfile:1@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183
# (dockerfile frontend 1.29.0, multi-arch index digest.) Needs BuildKit: RUN --mount
# and $BUILDPLATFORM. Podman/buildah ignore the syntax line and support both.
#
# imapvault container image: one static binary on distroless, running as nonroot (65532).
# imapvault is a one-shot tool, so the image has no default command: every run says
# `--account NAME backup` (or another command). No config, password, key or repository
# is baked in: mount /config (read-only) and /repo. See docs/DEPLOY.md.
#
# The build stage runs on the build machine's own platform and cross-compiles
# (pure Go, CGO off), so a multi-arch build needs no QEMU:
#   docker buildx build --platform linux/amd64,linux/arm64 -t imapvault .

# golang:1.26 (1.26.11), multi-arch index digest
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26@sha256:4165ee6bf3b7811fd7b40373290405c83d0167dcb87ee5f29c427cdcef45ff8f AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
# What `imapvault --version` reports. Declared late so that changing them only
# rebuilds the compile step.
ARG VERSION=dev
ARG COMMIT=
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/imapvault ./cmd/imapvault

# gcr.io/distroless/static-debian12:nonroot, multi-arch index digest.
# Has CA certificates and tzdata, no shell.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
# Version and revision labels are added at build time (`make docker-build`, CI):
# --label org.opencontainers.image.version=... (a build-arg here would be cached
# stale by podman/buildah).
LABEL org.opencontainers.image.title="imapvault" \
      org.opencontainers.image.description="IMAP backup with point-in-time snapshots, mass-deletion alerts and restore" \
      org.opencontainers.image.source="https://github.com/michele/imapvault" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/imapvault /imapvault
COPY LICENSE /LICENSE
# `--account NAME` then reads /config/imapvault/NAME.toml (XDG_CONFIG_HOME=/config).
ENV XDG_CONFIG_HOME=/config
USER 65532:65532
ENTRYPOINT ["/imapvault"]
CMD ["--help"]
