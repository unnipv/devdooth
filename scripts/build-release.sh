#!/usr/bin/env bash
# Build release archives for every supported target into ./dist.
#
#   ./scripts/build-release.sh v0.1.0
#
# Used by the release workflow and runnable locally to verify cross-compilation.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${1:-${VERSION:-0.0.0-dev}}"
LDFLAGS="-s -w -X main.version=${VERSION} -X github.com/unnipv/devdooth/internal/worker.Version=${VERSION}"

rm -rf dist
mkdir -p dist

for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do
  GOOS="${target%/*}"
  GOARCH="${target#*/}"
  ext=""
  [ "$GOOS" = "windows" ] && ext=".exe"
  base="devdooth_${VERSION}_${GOOS}_${GOARCH}"

  echo "building ${base}"
  mkdir -p "dist/${base}"
  CGO_ENABLED=0 GOOS="${GOOS}" GOARCH="${GOARCH}" go build -trimpath \
    -ldflags "${LDFLAGS}" -o "dist/${base}/devdooth${ext}" ./cmd/devdooth
  cp README.md LICENSE "dist/${base}/"

  if [ "$GOOS" = "windows" ]; then
    (cd dist && zip -q -r "${base}.zip" "${base}")
  else
    tar -C dist -czf "dist/${base}.tar.gz" "${base}"
  fi
  rm -rf "dist/${base}"
done

if command -v sha256sum >/dev/null 2>&1; then
  (cd dist && sha256sum -- *.tar.gz *.zip > checksums.txt)
else
  (cd dist && shasum -a 256 -- *.tar.gz *.zip > checksums.txt)
fi

echo
echo "artifacts in ./dist:"
ls -1 dist
