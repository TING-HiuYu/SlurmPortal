# Slurm Portal slave for one cluster, plus the host bridge that lets its triggers run commands on
# the cluster host over SSH. Build from the repository root:
#   docker build -f deploy/docker/slave.Dockerfile -t slurm-portal-slave .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/slurm-portal-slave ./cmd/slave \
    && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/slurm-portal-host-bridge ./cmd/host-bridge

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata openssh-client \
    && install -d -m 0700 -o 10001 -g 10001 /var/lib/slurm-portal
COPY --from=build /out/ /usr/local/bin/
USER 10001:10001
WORKDIR /var/lib/slurm-portal
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s CMD wget -qO- http://127.0.0.1:8081/healthz >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/slurm-portal-slave", "-config", "/etc/slurm-portal/slave.yml"]
