#!/usr/bin/env bash
# Generates a CycloneDX SBOM for a Go module in this repo — the dependency
# graph of what actually ships in the binary, not a hand-maintained list
# that drifts. Requires cyclonedx-gomod (pure Go, no cgo), pinned to the newest
# release that builds with the Go version this repository declares — @latest is
# v1.12.0 and needs Go 1.26:
#   go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.10.0
#
#     ./scripts/gen_sbom.sh sbom.json              # the kernel, the default
#     ./scripts/gen_sbom.sh media-sbom.json media  # the media subsystem
#
# The module is an argument because this repository ships two binaries with two
# version lines, and one SBOM covering "the repo" would describe neither: the
# media module depends on a Postgres driver the kernel has never heard of.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:-$root/sbom.json}"
module="${2:-kernel}"

if [ ! -f "$root/$module/go.mod" ]; then
  echo "no Go module at $module/ (expected $root/$module/go.mod)" >&2
  exit 1
fi

if ! command -v cyclonedx-gomod >/dev/null 2>&1; then
  echo "cyclonedx-gomod not found on PATH." >&2
  echo "install: go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.10.0" >&2
  exit 1
fi

(cd "$root/$module" && cyclonedx-gomod mod -json -output "$out")
echo "wrote $out (module: $module)"
