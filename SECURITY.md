# Security Policy

AURA is pre-1.0. There is no version support matrix yet — `main` and the
latest tagged release are what get security fixes. If you're running an
older release, update before reporting; we won't backport.

## Reporting a vulnerability

Use [GitHub Security Advisories](https://github.com/DeepAxiom/deepaxiom_aura/security/advisories/new)
for anything you wouldn't want public before a fix ships. That's the
private channel — it notifies maintainers directly and lets us coordinate a
fix before disclosure. Please don't open a public issue for a vulnerability
you haven't already coordinated privately.

There is no bug bounty program. We'll credit you in the advisory and the
fix's changelog entry unless you ask not to be named.

We don't have a fixed SLA yet — this is a young project maintained without
a dedicated security team. Expect an initial response in a few days, not
hours.

## Scope

In scope: `kernel/`, `spec/`, `sdk/`, and the first-party skills under
`skills/`.

Out of scope: third-party skills. A skill is untrusted code that connects
to a node; this project reviews the ones it ships, not ones you install
from elsewhere. If a third-party skill can do something a skill shouldn't
be able to do *because the kernel let it*, that's in scope — the kernel's
enforcement is what we're accountable for, not the skill's intent.

## What counts as a vulnerability here

This project's own threat model ([Security model](GUIDE.md#security-model)) is
the baseline — anything that breaks one of these is a security
bug, not a feature request:

- **Authorization gate bypass** — a graph capability running despite
  `aura.policy.yaml` denying it, or a graph exempting itself from the gate
  (`"gate": "none"`) without the node's policy explicitly granting that.
  See [`spec/c1-manifest.md`](spec/c1-manifest.md).
- **Ledger integrity bypass** — an altered or forged effect ledger entry
  that `aura verify` fails to detect, including a chain repaired after
  tampering (only the checkpoint's Ed25519 signature is supposed to catch
  that). See [`spec/c4-ledger.md`](spec/c4-ledger.md).
- **Wasm sandbox escape** — a skill running under `wazero` reading,
  writing, or reaching the network outside what its manifest's
  `filesystem`/`egress_http` permissions declare.
- **Control-plane auth bypass** — reaching `/v1/*`, `/ws/*`, `/mcp`, or the
  A2A card without the bearer token on a node that didn't opt out of auth.
- **Replay or idempotency violation** — a captured, signed ingress delivery
  that re-executes a graph, or a retried envelope that isn't deduplicated
  by `idem` on either side of a federation link. See
  [`spec/c3-channel.md`](spec/c3-channel.md).

Reports about the *default* policy being permissive, or about behavior a
node's own `aura.policy.yaml` explicitly allows, aren't vulnerabilities —
policy is the node operator's decision, not the kernel's to override.

## Verifying a release

This repository releases **two artefacts on two tag lines**, each signed by the
workflow that built it:

| Artefact | Tag | Workflow | Files |
|---|---|---|---|
| `aura` — the kernel | `v0.3.0` | `.github/workflows/release.yml` | `aura-<os>-<arch>`, `sbom.json` |
| `aura-media` — the media subsystem | `media/v0.1.0` | `.github/workflows/release-media.yml` | `aura-media-<os>-<arch>`, `media-sbom.json` |

They are separate because their versions mean different things: a codec CVE
moves the media line and must leave the kernel's alone, which is what makes
pinning a kernel worth doing at all. Each release carries its own SBOM, because
the two modules do not share a dependency graph.

Both are built for six platforms, hashed into a `SHA256SUMS` file, and both the
checksums and the SBOM are signed keylessly with
[cosign](https://github.com/sigstore/cosign) via GitHub Actions' OIDC identity —
no long-lived private key exists for anyone to compromise. To verify a
downloaded binary:

```sh
# 1. Confirm the checksum
sha256sum -c SHA256SUMS --ignore-missing

# 2. Confirm SHA256SUMS itself was signed by this repo's release workflow.
#    The identity names the workflow file, so swap release.yml for
#    release-media.yml when verifying an aura-media download: a kernel
#    signature on a media artefact is exactly the substitution this catches.
cosign verify-blob \
  --certificate-identity-regexp "^https://github.com/DeepAxiom/deepaxiom_aura/.github/workflows/release.yml@.*$" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --signature SHA256SUMS.sig \
  --certificate SHA256SUMS.pem \
  SHA256SUMS
```

A successful `cosign verify-blob` proves the checksums came from this
repository's own release workflow, not from a fork or a tampered upload —
verify that *before* trusting `sha256sum -c`, not after.
