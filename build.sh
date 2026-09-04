#!/bin/sh
# build.sh: cross-compile a static linux/amd64 binary.
set -eu
cd "$(dirname "$0")"
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/sub2cc-quota-scheduler .
shasum -a 256 dist/sub2cc-quota-scheduler
