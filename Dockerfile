# syntax=docker/dockerfile:1.7
#
# Production image for summarizer, consumed by GoReleaser dockers_v2.
#
# GoReleaser pre-builds the binaries for every target platform and
# lays them out under <workdir>/<TARGETPLATFORM>/<binary> in the
# build context. This Dockerfile only does the final COPY + labels;
# it does NOT build Go code itself (see goreleaser.yaml warning
# "Don't build binaries in your Dockerfile" for why we avoid that).
#
# Multi-arch notes:
#   - $TARGETPLATFORM is the runtime platform the image is built
#     for; buildx resolves it at build time per architecture.
#   - The base image (gcr.io/distroless/static:nonroot) is a
#     manifest list, so buildx pulls the right arch variant
#     automatically.
#
# Runtime base:
#   - gcr.io/distroless/static:nonroot ships ca-certificates
#     (required for HTTPS to GitLab and the LLM API endpoints) and
#     nothing else. Runs as uid 65532 by default.

FROM gcr.io/distroless/static:nonroot AS runtime

# Per-platform binary lives at $TARGETPLATFORM/summarizer inside
# the build context that GoReleaser constructs.
ARG TARGETPLATFORM
COPY --chown=65532:65532 ${TARGETPLATFORM}/summarizer /usr/local/bin/summarizer

# OCI labels are written by GoReleaser at build time (see
# .goreleaser.yaml dockers_v2.annotations), so this Dockerfile stays
# free of build-arg sprawl and works identically for any tag/version.

# OCI exec form so the binary gets argv[0] rather than "sh -c".
# `docker run --rm ghcr.io/inful/summarizer --help` and
# `docker run --rm ghcr.io/inful/summarizer version` both work.
ENTRYPOINT ["/usr/local/bin/summarizer"]