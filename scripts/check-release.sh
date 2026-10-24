#!/bin/sh
set -eu

if [ "$#" -gt 2 ]; then
  echo 'Usage: sh scripts/check-release.sh [DIST_DIRECTORY] [EXPECTED_VERSION]' >&2
  exit 2
fi
release_dir=$(CDPATH='' cd "${1:-dist}" && pwd)
repo_root=$(CDPATH='' cd "$(dirname "$0")/.." && pwd)
expected_version=${2:-}
require_signed=${ZALKI_REQUIRE_SIGNED:-0}
if [ "$require_signed" = 1 ] && [ "$(uname -s)" != Darwin ]; then
  echo 'Release signature verification requires macOS' >&2
  exit 1
fi
set -- "$release_dir"/zalki_*_source.tar.gz
if [ "$#" -ne 1 ] || [ ! -f "$1" ]; then
  echo 'Expected exactly one Zalki source archive' >&2
  exit 1
fi
source_archive=$1
release_version=${source_archive##*/}
release_version=${release_version#zalki_}
release_version=${release_version%_source.tar.gz}
if [ -n "$expected_version" ] && [ "$release_version" != "$expected_version" ]; then
  echo 'Archive version does not match the release tag' >&2
  exit 1
fi

task_tmp=$(mktemp -d "${TMPDIR:-/tmp}/zalki-release.XXXXXX")
trap 'rm -rf "$task_tmp"' EXIT HUP INT TERM
(cd "$release_dir" && shasum -a 256 --check checksums.txt)

require_checksum() (
  checksum_line=$(cd "$release_dir" && shasum -a 256 "$1")
  if ! grep -Fxq "$checksum_line" "$release_dir/checksums.txt"; then
    echo "Missing checksum entry: $1" >&2
    exit 1
  fi
)
require_checksum "${source_archive##*/}"

tar -tzf "$source_archive" > "$task_tmp/source-files"
for required in go.mod go.sum cmd/zalki/main.go LICENSE NOTICE THIRD_PARTY_NOTICES.txt; do
  if ! awk -v name="$required" '$0 == name || substr($0, length($0) - length(name)) == "/" name { found = 1 } END { exit !found }' "$task_tmp/source-files"; then
    echo "Missing source file: $required" >&2
    exit 1
  fi
done

for arch in amd64 arm64; do
  archive="$release_dir/zalki_${release_version}_darwin_${arch}.tar.gz"
  require_checksum "${archive##*/}"
  tar -tzf "$archive" > "$task_tmp/binary-files"
  for required in zalki LICENSE NOTICE THIRD_PARTY_NOTICES.txt README.md completions/zalki.bash completions/_zalki completions/zalki.fish; do
    if ! grep -Fxq "$required" "$task_tmp/binary-files"; then
      echo "Missing $required in $archive" >&2
      exit 1
    fi
  done
  if [ "$require_signed" = 1 ]; then
    signature_dir="$task_tmp/signature-$arch"
    mkdir "$signature_dir"
    tar -xzf "$archive" -C "$signature_dir" zalki
    if ! codesign --verify --strict "$signature_dir/zalki" > "$signature_dir/verification" 2>&1 ||
       ! codesign -d --verbose=4 "$signature_dir/zalki" > "$signature_dir/details" 2>&1 ||
       ! grep -Fq 'Authority=Developer ID Application:' "$signature_dir/details" ||
       ! grep -Eq 'flags=.*runtime' "$signature_dir/details" ||
       ! grep -Eq '^Timestamp=' "$signature_dir/details"; then
      echo "Missing or invalid Developer ID signature, hardened runtime, or timestamp for $arch" >&2
      exit 1
    fi
  fi
done

sh "$repo_root/scripts/homebrew-cask.sh" "$release_version" \
  "$release_dir/zalki_${release_version}_darwin_arm64.tar.gz" \
  "$release_dir/zalki_${release_version}_darwin_amd64.tar.gz" > "$task_tmp/zalki.rb"
ruby -c "$task_tmp/zalki.rb"
mv "$task_tmp/zalki.rb" "$release_dir/zalki.rb"
(
  cd "$release_dir"
  awk '$2 != "zalki.rb"' checksums.txt > "$task_tmp/checksums.txt"
  shasum -a 256 zalki.rb >> "$task_tmp/checksums.txt"
  mv "$task_tmp/checksums.txt" checksums.txt
  shasum -a 256 --check checksums.txt
)

if [ "$(uname -s)" = Darwin ]; then
  case "$(uname -m)" in
    arm64) native_arch=arm64 ;;
    x86_64) native_arch=amd64 ;;
    *) echo 'Unsupported macOS test architecture' >&2; exit 1 ;;
  esac
  tar -xzf "$release_dir/zalki_${release_version}_darwin_${native_arch}.tar.gz" -C "$task_tmp" zalki
  actual_version=$(ZALKI_LOG_LEVEL=off "$task_tmp/zalki" version)
  case "$actual_version" in
    "$release_version"|"$release_version"+*) ;;
    *) echo 'Packaged binary reports the wrong version' >&2; exit 1 ;;
  esac
  ZALKI_LOG_LEVEL=off "$task_tmp/zalki" --help > "$task_tmp/help"
  grep -Fq 'Local messaging' "$task_tmp/help"
  echo "Native macOS $native_arch package smoke check passed."
fi
echo "Release $release_version archives, checksums, and cask verified."
