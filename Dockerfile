# Multi-stage build for all three statfs-guard binaries.
#
#   manager : cluster-wide controller + reports + alerts
#   agent   : per-node DaemonSet that reads rstat and revalidates the cache
#   prober  : the one-shot primitive both of the above are built on, handy for
#             `kubectl debug` and for proving the mechanism by hand
FROM golang:1.25 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/manager ./cmd/manager \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/agent   ./cmd/agent \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w"                                -o /out/prober  ./cmd/prober

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 65532 nonroot
COPY --from=build /out/manager /out/agent /out/prober /usr/local/bin/

# Neither binary changes data: the agent only reads statfs(2) and ceph.dir.*
# xattrs on a read-only hostPath, and the manager only reads the API.
#
# USER nonroot is the identity the *manager* runs as, and what satisfies
# runAsNonRoot in config/manager/deployment.yaml.
#
# The agent deliberately does NOT run as nonroot. config/agent/daemonset.yaml
# sets runAsUser: 0 in the container securityContext, which overrides this
# image-level USER. It needs root to traverse kubelet's volume directories and
# to read the ceph.dir.* xattrs on them; the hostPath it reads is mounted
# readOnly and it opens nothing for writing.
USER nonroot
ENTRYPOINT ["/usr/local/bin/manager"]
