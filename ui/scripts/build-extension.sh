#!/usr/bin/env bash
# scripts/build-extension.sh
#
# Drives Rancher's own @rancher/shell/scripts/build-pkg.sh (the same way
# every real extension does — see ui-plugin-examples/package.json), then
# rearranges its output into the directory layout that Rancher's
# `ui-plugin-catalog` controller expects:
#
#   dist-pkg/extensions/fusioncompute/0.2.0/
#   ├── files.txt                    ← manifest, one relative path per line
#   └── plugin/
#       ├── fusioncompute-0.2.0.umd.min.js
#       ├── fusioncompute-0.2.0.umd.min.js.map
#       └── package.json
#
# Reference: https://github.com/rancher/ui-plugin-examples/tree/main/extensions/node-driver/1.0.4
#
# Usage: bash ./scripts/build-extension.sh   (run from anywhere)
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
cd "$HERE"

PKG_NAME="fusioncompute"
VERSION="$(node -p "require('./pkg/${PKG_NAME}/package.json').version")"

echo "==> Building ${PKG_NAME}@${VERSION}"

# Official build: creates the `pkg/<name>/.shell` symlink, runs
#   vue-cli-service build --name <pkg>-<version> --target lib index.js \
#     --dest dist-pkg/<pkg>-<version> --formats umd-min --filename <pkg>-<version>
# and post-processes dist-pkg/<pkg>-<version>/package.json via pkgfile.js.
bash ./node_modules/@rancher/shell/scripts/build-pkg.sh "${PKG_NAME}"

BUILT="dist-pkg/${PKG_NAME}-${VERSION}"
if [ ! -d "${BUILT}" ]; then
  echo "ERROR: expected build output ${BUILT} was not produced" >&2
  ls -la dist-pkg/ 2>/dev/null || true
  exit 1
fi

# Rearrange into <version>/plugin/ + files.txt.
OUT="dist-pkg/extensions/${PKG_NAME}/${VERSION}"
rm -rf "${OUT}"
mkdir -p "${OUT}/plugin"
cp -r "${BUILT}/." "${OUT}/plugin/"

# Bundle-analyzer artefact, not part of the extension.
rm -f "${OUT}/plugin/report.html"

# files.txt — relative paths, sorted, no leading "./".
( cd "${OUT}" && find plugin -type f | sort ) > "${OUT}/files.txt"

echo "==> Wrote ${OUT}/"
echo "    files.txt:"
sed 's/^/      /' "${OUT}/files.txt"
