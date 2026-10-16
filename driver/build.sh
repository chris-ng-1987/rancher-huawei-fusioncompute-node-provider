#!/usr/bin/env bash
# build.sh — produce the docker-machine-driver-fusioncompute binary.
#
# Usage:   ./build.sh                 # build for the host architecture
#          GOOS=linux GOARCH=arm64 ./build.sh   # cross-compile
#          ./build.sh clean           # remove build artifacts
#
# The output binary must be named `docker-machine-driver-<name>` and live
# somewhere on $PATH so that `docker-machine create -d fusioncompute` can
# find it.

set -euo pipefail

BIN_NAME="docker-machine-driver-fusioncompute"
PKG="github.com/chris-ng-1987/rancher-huawei-fusioncompute-node-provider/driver"

case "${1:-build}" in
  clean)
    rm -f "${BIN_NAME}"
    rm -rf vendor/
    exit 0
    ;;
  build|"")
    echo "==> Tidying modules"
    go mod tidy
    echo "==> Building ${BIN_NAME} (${PKG})"
    go build -o "${BIN_NAME}" .
    echo "==> Built ./${BIN_NAME}"
    echo "    Move it onto your PATH, e.g.:"
    echo "      sudo mv ${BIN_NAME} /usr/local/bin/"
    ;;
  *)
    echo "Unknown command: $1" >&2
    exit 2
    ;;
esac
