import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import { createIssue, getArtifactText } from "./api";

/** Go's reading of stored markdown: written as a new issue's spec, parsed and rendered again. */
export async function goReadBack(markdown: string): Promise<string> {
  const issue = await createIssue({ project: "CORE", spec: markdown, title: "Read back" });
  return (await getArtifactText(issue.primary_artifact_id)).markdown;
}

/** The headless engine's reading of stored markdown: each table's rows, as their cells' text. */
export function engineTables(markdown: string): string[][][] {
  const script = fileURLToPath(new URL("./engine-tables.ts", import.meta.url));
  return JSON.parse(execFileSync("bun", [script], { encoding: "utf8", input: markdown }));
}
