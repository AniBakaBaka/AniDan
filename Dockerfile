# syntax=docker/dockerfile:1
# AniDan: pure-Go backend and the AGPL-3.0-derived Misaka frontend.
# Multi-platform builds: docker buildx build --platform linux/amd64,linux/arm64 .
ARG NODE_VERSION=24
ARG GO_VERSION=1.27

FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-bookworm-slim AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json web/.npmrc ./
RUN npm ci --ignore-scripts --no-audit --no-fund
COPY web/ ./
ARG ANIDAN_SOURCE_URL=/source-code
ENV VITE_ANIDAN_SOURCE_URL=${ANIDAN_SOURCE_URL}
RUN VITE_ANIDAN_SOURCE_URL="${ANIDAN_SOURCE_URL:-/source-code}" npm run build

# Archive the same allowlisted source context used for this build. No daemon,
# runtime configuration, downloaded comment pools or dependency caches enter it.
FROM --platform=$BUILDPLATFORM python:3.12-alpine AS corresponding-source
WORKDIR /src
COPY Dockerfile .dockerignore Makefile README.md LICENSE LICENSING.md SOURCE_DISTRIBUTION.md THIRD_PARTY_NOTICES.md go.mod go.sum compose.yaml ./
COPY .github/workflows/docker.yml ./.github/workflows/docker.yml
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web/ ./web/
COPY static/ ./static/
COPY scripts/ ./scripts/
COPY LICENSES/ ./LICENSES/
RUN python3 scripts/package-source.py --root /src --output /out/anidan-source.tar.gz

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY --from=corresponding-source /out/anidan-source.tar.gz /tmp/anidan-source.tar.gz
ARG TARGETOS=linux
ARG TARGETARCH
# Keep compiled Go packages across source edits and both target architectures.
# CI persists this mount separately; the GHA layer exporter does not include it.
RUN --mount=type=cache,id=anidan-go-build,target=/root/.cache/go-build \
    source_sha="$(sha256sum /tmp/anidan-source.tar.gz | cut -d ' ' -f 1)" \
    && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -buildvcs=false -trimpath -ldflags="-s -w -X github.com/AniBakaBaka/AniDan/internal/server.SourceArchiveSHA256=${source_sha}" -o /out/anidan ./cmd/anidan

FROM alpine:3.22 AS runtime
LABEL org.opencontainers.image.title="AniDan" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 anidan \
    && adduser -S -D -H -u 10001 -G anidan anidan \
    && mkdir -p /app /data \
    && chown 10001:10001 /app /data
WORKDIR /app
COPY --from=backend /out/anidan /app/anidan
COPY --from=frontend /src/web/dist/ /app/web/dist/
COPY static/ /app/static/
COPY LICENSE /app/LICENSE
COPY THIRD_PARTY_NOTICES.md /app/THIRD_PARTY_NOTICES.md
COPY LICENSES/ /app/LICENSES/
COPY LICENSING.md SOURCE_DISTRIBUTION.md /app/
COPY --from=corresponding-source --chmod=0644 /out/anidan-source.tar.gz /app/source/anidan-source.tar.gz
ENV ANIDAN_LISTEN=:7769 \
    ANIDAN_CONFIG_DIR=/data \
    TZ=Asia/Shanghai
USER 10001:10001
EXPOSE 7769
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 \
    CMD wget -q -O /dev/null "${ANIDAN_HEALTHCHECK_URL:-http://127.0.0.1:7769/api/health}" || exit 1
ENTRYPOINT ["/app/anidan"]
CMD ["-setup"]
