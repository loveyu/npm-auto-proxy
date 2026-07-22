#!/bin/bash
set -e

VERSION="${1:-dev}"
BINARY="npm-auto-proxy"

echo "Building ${BINARY} version=${VERSION}..."
go build -ldflags "-X main.version=${VERSION}" -o "${BINARY}" .
echo "Build complete: ${BINARY}"
