# License

**Deep Axiom** dual-licenses this repository by component. This file is the map;
the enforceable text lives in [`LICENSE-AGPL-3.0.txt`](LICENSE-AGPL-3.0.txt) and
[`LICENSE-APACHE-2.0.txt`](LICENSE-APACHE-2.0.txt), unmodified from the canonical
FSF/ASF originals. The main source directories (`kernel/`, `media/`, `ui/`,
`sdk/`, `skills/`, `spec/`) also carry their own short `LICENSE` file pointing back here,
so the applicable terms are never more than one directory away from the code
they cover.

## Why two licenses

The standard (the protocol) and the product (the kernel that implements it) have
different jobs, so they carry different licenses:

- The **standard must stay unconditionally open**, or the whole pitch of a
  neutral, federable ecosystem collapses. Anyone — including a competitor —
  must be free to build a conformant kernel, SDK, or skill against it with zero
  friction and zero obligation. That is `spec/` and `sdk/`.
- The **kernel is the thing a cloud provider could otherwise take, host, and
  sell back to our own users without ever contributing anything back.**
  AGPLv3's network-use clause exists for exactly this shape of risk: if you run
  a modified copy of the kernel as a network service, you owe your users the
  source of your modifications. That is enough friction to deter silent
  strip-mining without closing off self-hosting, auditing, or contribution.

## Component map

| Path | License | SPDX |
|---|---|---|
| `spec/` — the C1/C2/C3 contracts, schemas, conformance suite | Apache-2.0 | `Apache-2.0` |
| `sdk/` — the skill-authoring SDK(s) | Apache-2.0 | `Apache-2.0` |
| `scripts/` — build/release tooling | Apache-2.0 | `Apache-2.0` |
| `kernel/` — the `aura` binary (the AURA kernel architecture) | AGPL-3.0-or-later¹ | `AGPL-3.0-or-later` |
| `ui/` — the control-plane frontend, embedded into the kernel binary | AGPL-3.0-or-later¹ | `AGPL-3.0-or-later` |
| `media/` — the `aura-media` binary (the media subsystem: transcode, package, frames) | AGPL-3.0-or-later¹ | `AGPL-3.0-or-later` |
| `skills/` — first-party skills (echo, llm-chat, asr, tts, sentence-chunker, planner, model-manager, memory-context, postgres-cdc) | AGPL-3.0-or-later¹ | `AGPL-3.0-or-later` |
| Documentation at the repo root (this file, `README.md`, `README-ES.md`, `GUIDE.md`, `GUIDE-ES.md`, `ROADMAP.md`, `ROADMAP-ES.md`, `CONTRIBUTING.md`) | Apache-2.0 | `Apache-2.0` |

¹ A commercial license is available in place of AGPLv3 for anyone who does not
want its obligations — see below. Third-party skills you write yourself against
the `sdk/` are **not** covered by this row: you license your own skill code
however you choose, provided it doesn't ship modified copies of `kernel/`,
`media/`, `ui/`, or the first-party skills.

**What this means in practice:**
- Writing and distributing your own skills (proprietary or open) against the
  `sdk/` — no AGPL obligation, ever. That is the entire point of keeping the
  SDK Apache-2.0: the ecosystem has to be safe for commercial skill authors.
- Self-hosting the kernel, modifying it, running it inside your own
  organization — no obligation to publish anything, AGPL only triggers on
  distributing or offering the modified software to others over a network.
- Taking the kernel, modifying it, and offering it as a hosted product to
  third parties — under AGPLv3 you must offer those users your modified
  source. If you don't want that obligation, buy the commercial license below.

## Commercial license

Deep Axiom offers a commercial license that replaces the AGPLv3 obligations on
`kernel/`, `media/`, `ui/`, and `skills/` with ordinary commercial terms (no source
disclosure requirement, no copyleft on your modifications). It exists for:

- Cloud/hosting providers who want to offer the kernel as a managed service
  without releasing their modifications.
- Companies embedding the kernel in a closed-source commercial product where
  AGPL's terms don't fit their distribution model.
- Anyone who wants the legal certainty of a signed commercial agreement rather
  than relying on their own AGPL compliance analysis.

**Nonprofits and foundations.** Registered nonprofits, academic institutions,
and open-source foundations may request the commercial license at no cost or
at a reduced rate, on request. The intent is that mission-driven organizations
should never be blocked by copyleft obligations they can't easily satisfy, nor
asked to pay commercial rates designed for companies monetizing the software.

To request a commercial or nonprofit license, contact
**diego.alberto.juarez@deepaxiom.com** with a short description of your use
case.

## Trademark

"Deep Axiom" is the umbrella product and company brand. "AURA" names the
kernel architecture specifically — the design of the binary in `kernel/` (the
contracts it implements, its execution model) — and is not a general-purpose
product name. Neither name may be used to imply endorsement of, or affiliation
with, a fork, a hosted offering, or a derivative product without the
trademark holder's permission. This restriction is trademark policy, not a
software license term — it does not limit what you may do with the code under
the licenses above, only what you may call it while doing so.

## Full license texts

- [`LICENSE-APACHE-2.0.txt`](LICENSE-APACHE-2.0.txt) — Apache License, Version
  2.0, unmodified.
- [`LICENSE-AGPL-3.0.txt`](LICENSE-AGPL-3.0.txt) — GNU Affero General Public
  License, Version 3, unmodified.

---

*This document is a summary and routing map; in any conflict, the license
texts in `LICENSE-APACHE-2.0.txt` and `LICENSE-AGPL-3.0.txt`, and the terms of
a signed commercial agreement where one exists, control.*
