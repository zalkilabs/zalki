#!/usr/bin/env bash
# Vet failures are report-only; incomplete package discovery is a setup failure.
set -euo pipefail

if [[ $# != 1 ]]; then
  echo "usage: windows-vet.sh REPORT_DIRECTORY" >&2
  exit 2
fi
report=$1
mkdir -p "$report"
summary_file=${GITHUB_STEP_SUMMARY:-"$report/summary.md"}

summary() { printf '%s\n' "$1" | tee -a "$summary_file"; }
discover() {
  local platform=$1
  local output="$report/$platform.txt"
  local diagnostic="$report/$platform-list.err"
  if ! go list ./... > "$output" 2> "$diagnostic" || [[ -s $diagnostic ]] || [[ ! -s $output ]]; then
    summary "package discovery failed or partial ($platform): $(head -n 1 "$diagnostic")"
    exit 1
  fi
}

discover host
GOOS=windows GOARCH=amd64 discover windows
failed=0
total=0
list=""
while read -r pkg; do
  total=$((total + 1))
  if ! GOOS=windows GOARCH=amd64 go vet "$pkg" > "$report/vet-$total.log" 2>&1; then
    failed=$((failed + 1))
    list="$list- $pkg"$'\n'
    echo "::warning::GOOS=windows go vet fails: $pkg"
    head -n 5 "$report/vet-$total.log"
  fi
done < "$report/windows.txt"
omitted="$(sort "$report/host.txt" | comm -23 - <(sort "$report/windows.txt") | paste -sd ' ' -)"
summary "GOOS=windows go vet: $failed of $total Windows-selected packages fail"
printf '%s' "$list" | tee -a "$summary_file"
summary "not built on windows: ${omitted:-none}"
