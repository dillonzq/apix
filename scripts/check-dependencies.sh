#!/usr/bin/env bash
set -euo pipefail
export GOWORK=off # Inspect this module independently of any local workspace.

# Inspect test imports as well as production imports. Standard-library packages
# have no Module; only packages belonging to the main module are allowed.
# Capture go list separately so an error cannot become a successful empty check.
third_party="$("${GO:-go}" list -deps -test -f '{{if .Module}}{{if not .Module.Main}}{{.ImportPath}}{{end}}{{end}}' ./...)"
if [[ "$third_party" =~ [^[:space:]] ]]; then
  printf 'Only the standard library and this module are allowed:\n%s\n' "$third_party" >&2
  exit 1
fi
printf '%s\n' 'Dependency graph (including tests) is standard library only.'
