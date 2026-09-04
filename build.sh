#!/bin/sh
# build.sh: cross-compile a static linux/amd64 binary; version from git describe.
set -eu
cd "$(dirname "$0")"
mkdir -p dist
VERSION="$(git describe --tags --always 2>/dev/null | sed 's/^v//')"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=${VERSION:-dev}" -o dist/sub2api-quota-scheduler .
shasum -a 256 dist/sub2api-quota-scheduler
