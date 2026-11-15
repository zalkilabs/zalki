#!/usr/bin/env bash
# Pure shell fixtures: PATH contains a fake Go; no compiler or platform vet runs.
set -euo pipefail

root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
reporter=$(cd "$(dirname "$0")" && pwd)/windows-vet.sh
mkdir "$root/bin"
cat > "$root/bin/go" <<'GO'
#!/usr/bin/env bash
set -eu
printf '%s %s\n' "${GOOS:-host}" "$*" >> "$FIXTURE_CALLS"
if [[ $1 == list ]]; then
  if [[ ${GOOS:-host} == "$FIXTURE_PLATFORM" ]]; then
    case $FIXTURE_FAILURE in
      partial) printf 'example.test/partial\n'; echo 'discovery failed' >&2; exit 1 ;;
      exit_only) printf 'example.test/partial\n'; exit 1 ;;
      stderr) printf 'example.test/partial\n'; echo 'discovery warning' >&2; exit 0 ;;
      empty) exit 0 ;;
    esac
  fi
  printf 'example.test/a\nexample.test/b\n'
  if [[ ${GOOS:-host} == host ]]; then printf 'example.test/host-only\n'; fi
elif [[ $1 == vet ]]; then
  if [[ $2 == example.test/b ]]; then echo 'fixture vet finding'; exit 1; fi
else
  echo 'unexpected fake-Go call' >&2
  exit 99
fi
GO
chmod +x "$root/bin/go"

run_reporter() {
  env -u GOOS -u GOARCH PATH="$root/bin:/usr/bin:/bin" GITHUB_STEP_SUMMARY="$1/summary.md" \
    FIXTURE_CALLS="$1/calls.txt" FIXTURE_PLATFORM="$2" FIXTURE_FAILURE="$3" \
    bash "$reporter" "$1" > "$1/output.log" 2>&1
}

positive="$root/positive"
mkdir "$positive"
run_reporter "$positive" none none
grep -Fx 'GOOS=windows go vet: 1 of 2 Windows-selected packages fail' "$positive/summary.md"
grep -Fx 'not built on windows: example.test/host-only' "$positive/summary.md"
grep -Fx 'windows vet example.test/a' "$positive/calls.txt"
grep -Fx 'windows vet example.test/b' "$positive/calls.txt"
if grep -q 'vet example.test/host-only' "$positive/calls.txt"; then
  echo 'host-only package was vetted as Windows' >&2; exit 1
fi

for platform in host windows; do
  for failure in partial exit_only stderr empty; do
    fixture="$root/$platform-$failure"
    mkdir "$fixture"
    if run_reporter "$fixture" "$platform" "$failure"; then
      echo "discovery control accepted $platform/$failure" >&2; exit 1
    fi
    grep -F "package discovery failed or partial ($platform):" "$fixture/summary.md"
    if grep -q ' vet ' "$fixture/calls.txt"; then
      echo "vet started after incomplete $platform/$failure discovery" >&2; exit 1
    fi
    echo "PASS discovery refuses $platform/$failure before vet"
  done
done
echo 'PASS Windows vet reporter controls (eight discovery negatives and report-only positive)'
