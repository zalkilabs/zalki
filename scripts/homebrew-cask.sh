#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
  echo "Usage: sh scripts/homebrew-cask.sh VERSION ARM64_ARCHIVE AMD64_ARCHIVE" >&2
  exit 2
fi
release_version=$1
arm64_archive=$2
amd64_archive=$3
if ! printf '%s\n' "$release_version" | LC_ALL=C grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'; then
  echo "VERSION must be a release version without a leading v" >&2
  exit 2
fi
if [ "$(basename "$arm64_archive")" != "zalki_${release_version}_darwin_arm64.tar.gz" ] ||
   [ "$(basename "$amd64_archive")" != "zalki_${release_version}_darwin_amd64.tar.gz" ]; then
  echo "Archives must match zalki_VERSION_darwin_ARCH.tar.gz in arm64, amd64 order" >&2
  exit 2
fi
archive_checksum() {
  checksum=$(shasum -a 256 "$1")
  checksum=${checksum%% *}
  if ! printf '%s\n' "$checksum" | LC_ALL=C grep -Eq '^[0-9a-f]{64}$'; then
    echo "Could not read an archive SHA-256 checksum" >&2
    return 2
  fi
  printf '%s' "$checksum"
}
arm64_checksum=$(archive_checksum "$arm64_archive")
amd64_checksum=$(archive_checksum "$amd64_archive")
repo_root=$(CDPATH='' cd "$(dirname "$0")/.." && pwd)
awk -v version="$release_version" -v arm64="$arm64_checksum" -v amd64="$amd64_checksum" '
  { gsub(/@VERSION@/, version); gsub(/@ARM64_SHA@/, arm64); gsub(/@AMD64_SHA@/, amd64); print }
' "$repo_root/packaging/homebrew/zalki.rb.tmpl"
