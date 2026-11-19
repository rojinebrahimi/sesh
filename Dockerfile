# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/rojinebrahimi/sesh/cmd.Version=${VERSION}" -o /out/sesh .

FROM gcr.io/distroless/static-debian12:nonroot
LABEL io.modelcontextprotocol.server.name="io.github.rojinebrahimi/sesh" \
      org.opencontainers.image.source="https://github.com/rojinebrahimi/sesh" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.description="Read-only MCP server over your coding-agent sessions"
# Session stores are mounted read-only under /data (see server.json); point each adapter at its mount.
ENV CLAUDE_CONFIG_DIR=/data/claude \
    CODEX_HOME=/data/codex \
    XDG_DATA_HOME=/data/share \
    OMP_CODING_AGENT_DIR=/data/omp \
    PI_CODING_AGENT_DIR=/data/pi
COPY --from=build /out/sesh /sesh
ENTRYPOINT ["/sesh"]
CMD ["mcp"]
