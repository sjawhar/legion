import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { setClassificationFixtureRecorder } from "../reducers";

const fixturesRoot = resolve(import.meta.dir, "../../../../contracts/fixtures/classification");

function canonicalize(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonicalize);
  if (value !== null && typeof value === "object") {
    const record = value as Record<string, unknown>;
    return Object.fromEntries(
      Object.keys(record)
        .sort()
        .filter((key) => record[key] !== undefined)
        .map((key) => [key, canonicalize(record[key])])
    );
  }
  return value;
}

function canonicalJson(value: unknown): string {
  return JSON.stringify(canonicalize(value));
}

if (process.env.LEGION_RECORD_FIXTURES === "1") {
  setClassificationFixtureRecorder((fn, input, output) => {
    const directory = resolve(fixturesRoot, fn);
    const hash = createHash("sha256").update(canonicalJson(input)).digest("hex");
    const fixture = resolve(directory, `${hash}.json`);
    const content = canonicalJson({ fn, input, output });
    mkdirSync(directory, { recursive: true });
    if (existsSync(fixture)) {
      if (readFileSync(fixture, "utf8") !== content) {
        throw new Error(`non-deterministic fixture for ${fn}: ${fixture}`);
      }
      return;
    }
    writeFileSync(fixture, content);
  });
}
