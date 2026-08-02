#!/usr/bin/env node
/**
 * Generate TypeScript types from the frozen JSON Schemas.
 *
 * spec/schemas/ is the single source of truth for C1, C2 and C3. Hand-writing
 * the same shapes in TypeScript would guarantee they drift — the docs already
 * drifted from the code once in this repo, and types are harder to notice.
 * Generating them means a schema change either updates the client or fails CI.
 *
 * This deliberately handles only the subset of JSON Schema those three files
 * use. A general-purpose generator would be a dependency and a lot of surface
 * for no extra benefit.
 *
 *   node scripts/generate-types.mjs           # write src/generated/types.ts
 *   node scripts/generate-types.mjs --check   # fail if it is out of date
 */
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const specDir = join(here, "..", "..", "..", "spec", "schemas");
const outFile = join(here, "..", "src", "generated", "types.ts");

const SOURCES = [
  { file: "envelope.schema.json", root: "Envelope" },
  { file: "manifest.schema.json", root: "Manifest" },
  { file: "graph-ir.schema.json", root: "GraphIR" },
];

/** Turn a $defs key or property name into a PascalCase type name. */
const pascal = (s) =>
  s.replace(/(^|[-_/])([a-z])/g, (_, __, c) => c.toUpperCase()).replace(/[^A-Za-z0-9]/g, "");

function tsType(schema, ctx, indent = "  ") {
  if (!schema || typeof schema !== "object") return "unknown";

  if (schema.$ref) {
    const name = schema.$ref.replace("#/$defs/", "");
    return ctx.defNames.get(name) ?? "unknown";
  }
  if (schema.const !== undefined) return JSON.stringify(schema.const);
  if (Array.isArray(schema.enum)) return schema.enum.map((v) => JSON.stringify(v)).join(" | ");

  switch (schema.type) {
    case "string":
      return "string";
    case "integer":
    case "number":
      return "number";
    case "boolean":
      return "boolean";
    case "array":
      return `${tsType(schema.items, ctx, indent)}[]`;
    case "object":
      return objectType(schema, ctx, indent);
    default:
      // A property with no declared type is genuinely "anything" in these
      // schemas (envelope.payload), not a gap in the generator.
      return "unknown";
  }
}

function objectType(schema, ctx, indent) {
  const props = schema.properties ?? {};
  const names = Object.keys(props);
  if (names.length === 0) return "Record<string, unknown>";
  const required = new Set(schema.required ?? []);
  const inner = indent + "  ";
  const lines = names.map((name) => {
    const optional = required.has(name) ? "" : "?";
    const key = /^[A-Za-z_$][A-Za-z0-9_$]*$/.test(name) ? name : JSON.stringify(name);
    return `${inner}${key}${optional}: ${tsType(props[name], ctx, inner)};`;
  });
  return `{\n${lines.join("\n")}\n${indent}}`;
}

function generate() {
  const parts = [
    "// GENERATED FILE — do not edit.",
    "//",
    "// Produced from spec/schemas/*.json by scripts/generate-types.mjs.",
    "// The frozen contracts are the source of truth; run `npm run generate`",
    "// after changing a schema. CI checks this file is current.",
    "",
  ];

  for (const { file, root } of SOURCES) {
    const schema = JSON.parse(readFileSync(join(specDir, file), "utf8"));
    const ctx = { defNames: new Map() };
    for (const name of Object.keys(schema.$defs ?? {})) {
      ctx.defNames.set(name, pascal(name));
    }

    parts.push(`/** ${schema.title} — ${schema.$id} */`);
    parts.push(`export interface ${root} ${objectType(schema, ctx, "")}`);
    parts.push("");

    for (const [name, def] of Object.entries(schema.$defs ?? {})) {
      const typeName = ctx.defNames.get(name);
      if (def.type === "array") {
        parts.push(`export type ${typeName} = ${tsType(def, ctx, "")};`);
      } else {
        parts.push(`export interface ${typeName} ${objectType(def, ctx, "")}`);
      }
      parts.push("");
    }
  }

  // The envelope's `kind` enum is the one thing callers switch on constantly,
  // so give it a name of its own rather than making them index the interface.
  parts.push('export type EnvelopeKind = Envelope["kind"];');
  parts.push("");
  return parts.join("\n");
}

const generated = generate();

if (process.argv.includes("--check")) {
  let current = "";
  try {
    current = readFileSync(outFile, "utf8");
  } catch {
    /* missing counts as out of date */
  }
  if (current !== generated) {
    console.error(
      "src/generated/types.ts is out of date with spec/schemas.\n" +
        "Run: npm run generate  (in sdk/node) and commit the result."
    );
    process.exit(1);
  }
  console.log("generated types are up to date with spec/schemas");
} else {
  writeFileSync(outFile, generated);
  console.log(`wrote ${outFile}`);
}
