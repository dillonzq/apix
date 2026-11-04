#!/usr/bin/env bash
set -euo pipefail
export GOWORK=off # Inspect this module independently of any local workspace.

# The one allowed third-party module: SIMD Base64 and order-preserving JSON
# for the request, response and stream hot paths. It has no dependencies.
allowed_module='github.com/MoeForge/moego'

# Inspect test imports as well as production imports. Standard-library packages
# have no Module; packages of the main module and the allowed module pass.
# Compare module paths exactly, so a similarly named sibling module (such as
# github.com/MoeForge/moego-extra) is rejected.
# Capture go list separately so an error cannot become a successful empty check.
third_party="$("${GO:-go}" list -deps -test -f '{{if .Module}}{{if not .Module.Main}}{{if ne .Module.Path "'"$allowed_module"'"}}{{.ImportPath}}{{end}}{{end}}{{end}}' ./...)"
if [[ "$third_party" =~ [^[:space:]] ]]; then
  printf 'Only the standard library, this module and %s are allowed:\n%s\n' "$allowed_module" "$third_party" >&2
  exit 1
fi
printf 'Dependency graph (including tests) is the standard library and %s only.\n' "$allowed_module"
