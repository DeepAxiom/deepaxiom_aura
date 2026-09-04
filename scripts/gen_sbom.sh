#!/usr/bin/env bash
# Generates a CycloneDX SBOM for the kernel's Go module — the dependency
# graph of what actually ships in the binary, not a hand-maintained list
# that drifts. Requires cyclonedx-gomod (pure Go, no cgo):
#   go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:-$root/sbom.json}"

if ! command -v cyclonedx-gomod >/dev/null 2>&1; then
  echo "cyclonedx-gomod not found on PATH." >&2
  echo "install: go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest" >&2
  exit 1
fi

(cd "$root/kernel" && cyclonedx-gomod mod -json -output "$out")
echo "wrote $out"
