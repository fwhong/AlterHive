# Source build. Secrets are excluded from the build context AND explicit runtime COPYs.
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /alterhive . && \
    mkdir -p /sockets && chmod 0750 /sockets && touch /sockets/.keep

FROM scratch
WORKDIR /app
COPY --from=builder /alterhive /app/alterhive
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /src/configs/topology.yaml /app/configs/topology.yaml
COPY --from=builder /src/configs/services/ssh.yaml /app/configs/services/ssh.yaml
COPY --from=builder --chown=10001:20000 /sockets/ /run/alterhive/decisions/
COPY --from=builder --chown=10002:20000 /sockets/ /run/alterhive/ingress/
USER 10002:20000
ENTRYPOINT ["/app/alterhive"]
