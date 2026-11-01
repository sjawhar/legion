import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import { createIssue, getArtifactText } from "./api";

/** Go's reading of stored markdown: written as a new issue's spec, parsed and rendered again. */
export async function goReadBack(markdown: string): Promise<string> {
  const issue = await createIssue({ project: "CORE", spec: markdown, title: "Read back" });
  return (await getArtifactText(issue.primary_artifact_id)).markdown;
}

/** The headless engine's reading of stored markdown, from engine-reading.ts. */
function engineReading(markdown: string): { tables: string[][][]; paragraphs: string[] } {
  const script = fileURLToPath(new URL("./engine-reading.ts", import.meta.url));
  return JSON.parse(execFileSync("bun", [script], { encoding: "utf8", input: markdown }));
}

/** The headless engine's reading of stored markdown: each table's rows, as their cells' text. */
export function engineTables(markdown: string): string[][][] {
  return engineReading(markdown).tables;
}

/** The headless engine's reading of stored markdown: each paragraph's text. */
export function engineParagraphs(markdown: string): string[] {
  return engineReading(markdown).paragraphs;
}
