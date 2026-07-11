# Build + run the Mesh team-sync hub (mesh-hub).
#
# Only the HUB is containerized. It authors the team's git history via os/exec,
# so git ships in the runtime image (the locked S1 decision). The CLIENT binary
# (mesh) stays a separate single static binary teammates install on their
# laptops; it never runs here and never needs git.

# ---------- build stage ----------
FROM golang:1.26.5-alpine AS builder
RUN apk add --no-cache git
WORKDIR /app
# Build tags select the edition: empty = fair-code core (public mirror builds work
# as-is), "pro" = team-sync hub + team web UI + ANN retrieval. The production
# hub compose sets MESH_BUILD_TAGS=pro via build args; forgetting it makes
# mesh-ui crash-loop at boot ("team mode requires the pro build").
ARG MESH_BUILD_TAGS=""
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -tags "$MESH_BUILD_TAGS" -o /mesh-hub ./cmd/mesh-hub

# Cross-compile the mesh CLIENT for every common platform, so the hub can serve
# ready-to-run binaries (closing the onboarding loop: invite link -> install ->
# join). Plus a SHA256SUMS for verification.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /dist && for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
      os="${t%/*}"; arch="${t#*/}"; ext=""; [ "$os" = "windows" ] && ext=".exe"; \
      CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -tags "$MESH_BUILD_TAGS" -o "/dist/mesh-$os-$arch$ext" ./cmd/mesh; \
    done && cd /dist && sha256sum mesh-* > SHA256SUMS

# ---------- runtime stage ----------
FROM alpine:3.21
# git: the hub commits via os/exec. ca-certificates + wget for the healthcheck.
RUN apk add --no-cache ca-certificates git wget && \
    adduser -D -u 1000 mesh && \
    mkdir -p /var/lib/mesh-hub && chown -R mesh:mesh /var/lib/mesh-hub

COPY --from=builder /mesh-hub /usr/local/bin/mesh-hub
COPY --from=builder /dist /usr/local/share/mesh-dist
COPY deploy/entrypoint.sh /usr/local/bin/mesh-hub-entrypoint
RUN chmod +x /usr/local/bin/mesh-hub-entrypoint

USER mesh
ENV MESH_HUB_REPO=/var/lib/mesh-hub/vault \
    MESH_HUB_ADDR=:8848 \
    MESH_HUB_GC_HORIZON=90 \
    MESH_HUB_DOWNLOAD_DIR=/usr/local/share/mesh-dist
EXPOSE 8848
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://localhost:8848/healthz || exit 1
ENTRYPOINT ["mesh-hub-entrypoint"]
