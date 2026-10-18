# Builds wakil-mcp, web UI included, as a small self-contained image.
#
# The front end is built first because the Go binary embeds it, so the runtime
# image needs nothing but a data directory.

# ---- web UI ----------------------------------------------------------------
FROM node:24-alpine AS webui

WORKDIR /src/web

# The manifest and lockfile first, so a dependency change is the only thing that
# busts the cache for the install layer.
COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
RUN npm run build


# ---- Go build --------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

# The UI this stage embeds is the one the previous stage just built.
COPY --from=webui /src/internal/server/webui/dist ./internal/server/webui/dist

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags "-s -w \
        -X github.com/shabilullah/wakil-mcp/internal/version.Version=${VERSION} \
        -X github.com/shabilullah/wakil-mcp/internal/version.Commit=${COMMIT} \
        -X github.com/shabilullah/wakil-mcp/internal/version.Date=${DATE}" \
      -o /out/wakil-mcp ./cmd/wakil-mcp


# ---- runtime ---------------------------------------------------------------
FROM alpine:3.22

# ca-certificates for HTTPS upstreams, tzdata so audit timestamps are local.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 wakil \
 && mkdir -p /data \
 && chown wakil:wakil /data

COPY --from=build /out/wakil-mcp /usr/local/bin/wakil-mcp

# MIT asks for the notice to travel with copies, and an image is a copy.
COPY LICENSE /licenses/LICENSE

# Standard OCI metadata, so an image scan or a registry UI can answer the
# obvious questions without unpacking the image.
LABEL org.opencontainers.image.title="wakil-mcp" \
      org.opencontainers.image.description="One daemon that fronts any number of MCP servers, for every harness" \
      org.opencontainers.image.source="https://github.com/shabilullah/wakil-mcp" \
      org.opencontainers.image.licenses="MIT"

# A fresh named or anonymous volume inherits this directory's owner, so the
# daemon can write its config and audit database without running as root.
VOLUME ["/data"]

# The data directory doubles as the config directory.
ENV WAKIL_CONFIG_DIR=/data

USER wakil

# Serving a container means serving the network, not just the loopback. The
# first start prints the admin and client tokens to the log.
EXPOSE 8765
ENTRYPOINT ["/usr/local/bin/wakil-mcp"]
CMD ["serve", "--config", "/data/config.json", "--bind", "0.0.0.0:8765"]

# The liveness probe is deliberately unauthenticated and says nothing but "ok".
#
# podman's default output format is OCI, which has no HEALTHCHECK field: it warns
# and drops this. Build with "podman build --format docker" to keep it.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8765/healthz || exit 1
