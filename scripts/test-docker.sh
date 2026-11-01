#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
mkdir -p .build/go-cache .build/go
SOURCE=$(mktemp -d "$ROOT/.build/test-source.XXXXXX")
trap 'rm -rf -- "$SOURCE"' EXIT HUP INT TERM
# Include Go sources and module metadata only: never config, DBs, credentials,
# .git metadata, the host home, or the Docker socket.
git ls-files --cached --others --exclude-standard -- '*.go' go.mod go.sum |
while IFS= read -r path; do
    mkdir -p "$SOURCE/$(dirname -- "$path")"
    cp -- "$path" "$SOURCE/$path"
done
IMAGE=golang@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437
docker run --rm --init --user "$(id -u):$(id -g)" \
    -e HOME=/tmp -e GOCACHE=/out/go-cache -e GOPATH=/out/go -e GOTOOLCHAIN=local \
    -v "$SOURCE:/src:ro" -v "$ROOT/.build:/out" -w /src "$IMAGE" \
    sh -c 'go version && go test -race -count=1 ./... && go vet ./... && go build -trimpath -o /out/gridscraper ./cmd/gridscraper'
