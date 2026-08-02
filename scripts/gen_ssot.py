#!/usr/bin/env python3
"""Generate the contract constants from spec/ into every language that uses them.

The version string used to live in three files with two different values, and
the five skill types were written out by hand in a Go regex, a Go slice, a
Python tuple and the JSON Schemas. Nothing compared them, so they could drift
apart silently — and the failure mode is not a build error but a skill that
registers against one kernel and is rejected by another.

    python scripts/gen_ssot.py           # write the generated files
    python scripts/gen_ssot.py --check   # fail if any is stale (CI)

Sources of truth:
    spec/VERSION      the product version
    spec/enums.yaml   the contract enumerations
"""
from __future__ import annotations

import argparse
import io
import json
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent
BANNER_LINE = "Code generated from spec/enums.yaml and spec/VERSION. DO NOT EDIT."
REGEN = "Regenerate with: python scripts/gen_ssot.py"


def read_lf(path: Path) -> str:
    """Read a file without translating line endings."""
    return path.read_text(encoding="utf-8", newline="")


def write_lf(path: Path, content: str) -> None:
    """Write with LF endings on every platform.

    Go tooling is the reason this is explicit. CI runs gofmt as a hard gate and
    also checks that these files are not stale; if generation emitted CRLF on a
    Windows checkout, gofmt would rewrite the file and the staleness check would
    then fail on gofmt's own output — two green-by-themselves gates that cannot
    both pass.
    """
    with io.open(path, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(content)


def load() -> tuple[str, dict]:
    version = (ROOT / "spec" / "VERSION").read_text(encoding="utf-8").strip()
    enums = yaml.safe_load((ROOT / "spec" / "enums.yaml").read_text(encoding="utf-8"))
    return version, enums


def names(entries: list[dict]) -> list[str]:
    return [e["name"] for e in entries]


def go_const_name(value: str) -> str:
    """`confirm_request` -> `ConfirmRequest`, `human-approval` -> `HumanApproval`."""
    return "".join(part.capitalize() for part in value.replace("-", "_").split("_"))


def go_const_block(prefix: str, entries: list[dict]) -> list[str]:
    """Emit an aligned `const (...)` block.

    The alignment is not cosmetic: CI runs gofmt as a hard gate *and* checks
    that this file is not stale, so unaligned output would make the two
    contradict each other — gofmt would rewrite the file and the staleness
    check would then fail on the result.
    """
    names = [f"{prefix}{go_const_name(e['name'])}" for e in entries]
    values = [f'"{e["name"]}"' for e in entries]
    name_width = max(len(n) for n in names)
    value_width = max(len(v) for v in values)

    lines = ["const ("]
    for name, value, entry in zip(names, values, entries):
        lines.append(
            f"\t{name.ljust(name_width)} = {value.ljust(value_width)} // {entry['description']}"
        )
    lines.append(")")
    return lines


def gen_go(version: str, enums: dict) -> str:
    types = enums["skill_types"]
    kinds = enums["envelope_kinds"]
    qos = enums["qos_classes"]
    formats = enums["skill_formats"]
    decisions = enums["policy_decisions"]
    outcomes = enums["effect_outcomes"]

    out: list[str] = [
        f"// {BANNER_LINE}",
        f"// {REGEN}",
        "",
        "// Package spec holds the contract constants shared by every kernel",
        "// package. It imports nothing, so any package may depend on it.",
        "package spec",
        "",
        "// Version is the product version (spec/VERSION).",
        f'const Version = "{version}"',
        "",
        "// EffectType is the skill type whose edges act on the world, and so the",
        "// one the authorization policy treats specially.",
        f'const EffectType = "{enums["effect_type"]}"',
        "",
        "// Skill types (C1).",
    ]
    out += go_const_block("Type", types)
    out += [
        "",
        "// SkillTypes is every value of C1 `type`, in declaration order.",
        "var SkillTypes = []string{" + ", ".join(f"Type{go_const_name(n)}" for n in names(types)) + "}",
        "",
        "// Skill formats (C1).",
    ]
    out += go_const_block("Format", formats)
    out += [
        "",
        "// SkillFormats is every value of C1 `format`.",
        "var SkillFormats = []string{" + ", ".join(f"Format{go_const_name(n)}" for n in names(formats)) + "}",
        "",
        "// Envelope kinds (C3).",
    ]
    out += go_const_block("Kind", kinds)
    out += [
        "",
        "// EnvelopeKinds is every value of C3 `kind`.",
        "var EnvelopeKinds = []string{" + ", ".join(f"Kind{go_const_name(n)}" for n in names(kinds)) + "}",
        "",
        "// QoS classes (C3 rule 4).",
    ]
    out += go_const_block("QoS", qos)
    out += [
        "",
        "// QoSClasses is every value of a C2 edge's `qos`.",
        "var QoSClasses = []string{" + ", ".join(f"QoS{go_const_name(n)}" for n in names(qos)) + "}",
        "",
        "// Policy decisions (C4).",
    ]
    out += go_const_block("Decision", decisions)
    out += [
        "",
        "// PolicyDecisions is every decision a node policy may reach.",
        "var PolicyDecisions = []string{"
        + ", ".join(f"Decision{go_const_name(n)}" for n in names(decisions))
        + "}",
        "",
        "// Effect outcomes (C4): what became of an effect the ledger sealed.",
    ]
    out += go_const_block("Outcome", outcomes)
    out += [
        "",
        "// EffectOutcomes is every outcome a sealed ledger entry may record.",
        "var EffectOutcomes = []string{"
        + ", ".join(f"Outcome{go_const_name(n)}" for n in names(outcomes))
        + "}",
        "",
        "// CapabilityPattern matches a C1 `capability`: <type>.<function>[.<sub>].",
        f'const CapabilityPattern = `^({"|".join(names(types))})(\\.[a-z0-9_-]+)+$`',
        "",
    ]
    return "\n".join(out)


def gen_python(version: str, enums: dict) -> str:
    def tup(entries: list[dict]) -> str:
        return "(\n" + "".join(f'    "{n}",\n' for n in names(entries)) + ")"

    return f'''"""{BANNER_LINE}

{REGEN}
"""
from __future__ import annotations

VERSION = "{version}"

#: The skill type whose edges act on the world.
EFFECT_TYPE = "{enums["effect_type"]}"

#: C1 `type` — the five kinds of skill.
SKILL_TYPES = {tup(enums["skill_types"])}

#: C1 `format` — how a skill is packaged.
SKILL_FORMATS = {tup(enums["skill_formats"])}

#: C3 `kind` — every envelope kind.
ENVELOPE_KINDS = {tup(enums["envelope_kinds"])}

#: C3 rule 4 — per-edge delivery classes.
QOS_CLASSES = {tup(enums["qos_classes"])}

#: C2 — what an edge may declare about approval.
GATES = {tup(enums["gates"])}

#: C4 — what a node policy may decide about an effect.
POLICY_DECISIONS = {tup(enums["policy_decisions"])}

#: C4 — what became of an effect the ledger sealed.
EFFECT_OUTCOMES = {tup(enums["effect_outcomes"])}

#: C1 `capability` — <type>.<function>[.<subtype>].
CAPABILITY_PATTERN = r"^({"|".join(names(enums["skill_types"]))})(\\.[a-z0-9_-]+)+$"
'''


def gen_typescript(version: str, enums: dict) -> str:
    def union(entries: list[dict]) -> str:
        return " | ".join(f'"{n}"' for n in names(entries))

    def arr(entries: list[dict]) -> str:
        return "[" + ", ".join(f'"{n}"' for n in names(entries)) + "]"

    return f"""// {BANNER_LINE}
// {REGEN}

export const VERSION = "{version}";

/** The skill type whose edges act on the world. */
export const EFFECT_TYPE = "{enums["effect_type"]}";

/** C1 `type` — the five kinds of skill. */
export type SkillType = {union(enums["skill_types"])};
export const SKILL_TYPES: readonly SkillType[] = {arr(enums["skill_types"])};

/** C1 `format` — how a skill is packaged. */
export type SkillFormat = {union(enums["skill_formats"])};
export const SKILL_FORMATS: readonly SkillFormat[] = {arr(enums["skill_formats"])};

/** C3 `kind` — every envelope kind. */
export type EnvelopeKind = {union(enums["envelope_kinds"])};
export const ENVELOPE_KINDS: readonly EnvelopeKind[] = {arr(enums["envelope_kinds"])};

/** C3 rule 4 — per-edge delivery classes. */
export type QoSClass = {union(enums["qos_classes"])};
export const QOS_CLASSES: readonly QoSClass[] = {arr(enums["qos_classes"])};

/** C2 — what an edge may declare about approval. */
export type Gate = {union(enums["gates"])};
export const GATES: readonly Gate[] = {arr(enums["gates"])};

/** C4 — what a node policy may decide about an effect. */
export type PolicyDecision = {union(enums["policy_decisions"])};
export const POLICY_DECISIONS: readonly PolicyDecision[] = {arr(enums["policy_decisions"])};

/** C4 — what became of an effect the ledger sealed. */
export type EffectOutcome = {union(enums["effect_outcomes"])};
export const EFFECT_OUTCOMES: readonly EffectOutcome[] = {arr(enums["effect_outcomes"])};
"""


def check_json_schemas(enums: dict) -> list[str]:
    """Verify the JSON Schemas agree with spec/enums.yaml.

    The schemas are *checked* rather than regenerated, deliberately. C1 calls
    them normative and they are hand-authored, with formatting and wording
    their authors chose; rewriting them mechanically would reformat the whole
    document to say the same thing, which is churn that hides the one line that
    actually changed. Verifying gets the same guarantee — the values cannot
    drift apart — without taking authorship away from the file.
    """
    problems: list[str] = []

    def compare(where: str, got, want) -> None:
        if got != want:
            problems.append(f"{where}: schema has {got!r}, spec/enums.yaml says {want!r}")

    schemas = ROOT / "spec" / "schemas"

    manifest = json.loads((schemas / "manifest.schema.json").read_text(encoding="utf-8"))
    props = manifest.get("properties", {})
    compare("manifest.schema.json properties.type.enum",
            props.get("type", {}).get("enum"), names(enums["skill_types"]))
    compare("manifest.schema.json properties.format.enum",
            props.get("format", {}).get("enum"), names(enums["skill_formats"]))

    envelope = json.loads((schemas / "envelope.schema.json").read_text(encoding="utf-8"))
    compare("envelope.schema.json properties.kind.enum",
            envelope.get("properties", {}).get("kind", {}).get("enum"),
            names(enums["envelope_kinds"]))

    ir = json.loads((schemas / "graph-ir.schema.json").read_text(encoding="utf-8"))
    edge = ir.get("$defs", {}).get("edge", {}).get("properties", {})
    if "gate" in edge:
        compare("graph-ir.schema.json edge.gate.enum", edge["gate"].get("enum"),
                names(enums["gates"]))
    if "qos" in edge:
        compare("graph-ir.schema.json edge.qos.enum", edge["qos"].get("enum"),
                names(enums["qos_classes"]))

    return problems


def targets() -> list[tuple[Path, str]]:
    version, enums = load()
    return [
        (ROOT / "kernel" / "internal" / "spec" / "spec.go", gen_go(version, enums)),
        (ROOT / "sdk" / "python" / "src" / "aura" / "_spec.py", gen_python(version, enums)),
        (ROOT / "sdk" / "node" / "src" / "generated" / "spec.ts", gen_typescript(version, enums)),
    ]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true",
                        help="fail if any generated file is stale")
    args = parser.parse_args()

    generated = targets()
    stale: list[str] = []
    for path, content in generated:
        rel = path.relative_to(ROOT).as_posix()
        current = read_lf(path) if path.exists() else None
        if current == content:
            continue
        if args.check:
            stale.append(rel)
            continue
        path.parent.mkdir(parents=True, exist_ok=True)
        write_lf(path, content)
        print(f"wrote {rel}")

    _, enums = load()
    drift = check_json_schemas(enums)

    if stale or drift:
        if stale:
            print("These generated files are stale:\n")
            for rel in stale:
                print(f"  {rel}")
            print(f"\n{REGEN}\n")
        if drift:
            print("These JSON Schemas disagree with spec/enums.yaml:\n")
            for problem in drift:
                print(f"  {problem}")
            print("\nThe schemas are normative and hand-authored, so fix them by hand —")
            print("or change spec/enums.yaml if the contract really did gain a value.")
        return 1

    if args.check:
        print(f"OK: {len(generated)} generated file(s) and 3 schema(s) match spec/.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
