# Slurm Portal master for container deployments: the native server (cmd/server, SQLite) and the
# web app it serves. Build from the repository root:
#   docker build -f deploy/docker/master.Dockerfile -t slurm-portal-master .
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npx vite build --logLevel warn

FROM golang:1.26-alpine AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/slurm-portal-master ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && install -d -m 0700 -o 10001 -g 10001 /data
COPY --from=server /out/slurm-portal-master /usr/local/bin/slurm-portal-master
COPY --from=web /src/web/dist /srv/web
ENV SYSTEM_ROLE=master \
    HTTP_LISTEN=0.0.0.0:8080 \
    STATIC_DIR=/srv/web \
    DB_DSN="file:/data/portal.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
USER 10001:10001
WORKDIR /data
EXPOSE 8080 8443
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/slurm-portal-master"]
