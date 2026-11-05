#!/bin/sh
# Build a production scratch image without downloading any Docker base image.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
IMAGE=${1:-alterhive:split}
ARCH=${TARGETARCH:-$(docker info --format '{{.Architecture}}')}
case "$ARCH" in aarch64|arm64) ARCH=arm64;; x86_64|amd64) ARCH=amd64;; *) echo 'unsupported target architecture' >&2; exit 1;; esac
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM
mkdir -p "$WORK/configs/services" "$WORK/sockets"
chmod 0750 "$WORK/sockets"
touch "$WORK/sockets/.keep"
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags='-s -w' -o "$WORK/alterhive" .)
cp "$ROOT/configs/topology.yaml" "$WORK/configs/"
cp "$ROOT/configs/services/ssh.yaml" "$WORK/configs/services/"
CERT=${SSL_CERT_FILE:-/etc/ssl/cert.pem}
[ -f "$CERT" ] || CERT=/etc/ssl/certs/ca-certificates.crt
[ -f "$CERT" ] || { echo 'set SSL_CERT_FILE to a public CA bundle' >&2; exit 1; }
cp "$CERT" "$WORK/ca-certificates.crt"
docker build --platform "linux/$ARCH" -f "$ROOT/Dockerfile.runtime" -t "$IMAGE" "$WORK"
