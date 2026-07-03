#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [[ -z "${VERSION:-}" ]]; then
  echo "VERSION is required. Example: VERSION=v0.1.0-alpha.1 task release:all" >&2
  exit 1
fi

DIST="$ROOT/dist"
rm -rf "$DIST"
mkdir -p "$DIST"

task generate

export CGO_ENABLED="${CGO_ENABLED:-0}"
export GOCACHE="${GOCACHE:-$ROOT/tmp/go-build-cache}"

targets=(
  "linux amd64"
  "linux arm64"
  "darwin amd64"
  "darwin arm64"
  "windows amd64"
)

for target in "${targets[@]}"; do
  read -r goos goarch <<< "$target"
  ext=""
  archive_ext="tar.gz"
  if [[ "$goos" == "windows" ]]; then
    ext=".exe"
    archive_ext="zip"
  fi

  name="gofer-${VERSION}-${goos}-${goarch}"
  package_dir="$DIST/$name"
  mkdir -p "$package_dir"

  echo "==> Building $goos/$goarch"
  GOOS="$goos" GOARCH="$goarch" go build -tags embedded_assets -trimpath -o "$package_dir/gofer$ext" .
  cp README.md LICENSE "$package_dir/"

  echo "==> Packaging $name.$archive_ext"
  if [[ "$archive_ext" == "zip" ]]; then
    (cd "$DIST" && zip -qr "$name.zip" "$name")
  else
    (cd "$DIST" && tar -czf "$name.tar.gz" "$name")
  fi

  rm -rf "$package_dir"
done

(
  cd "$DIST"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum *.tar.gz *.zip > checksums.txt
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 *.tar.gz *.zip > checksums.txt
  else
    echo "sha256sum or shasum is required to write checksums.txt" >&2
    exit 1
  fi
)

echo "Release artifacts written to $DIST"
ls -lh "$DIST"
