import { readFile, writeFile } from "node:fs/promises";

export const FRONT_MATTER_RE = /^---\n([\s\S]*?)\n---\n?/;

export interface LearningFrontMatter {
  date?: string;
  status?: string;
}

function extractField(frontMatter: string, field: "date" | "status"): string | undefined {
  const match = frontMatter.match(new RegExp(`^${field}:\\s*(.+)$`, "m"));
  return match?.[1]?.trim();
}

export async function readLearningFrontMatter(
  filePath: string
): Promise<LearningFrontMatter | null> {
  const contents = await readFile(filePath, "utf-8");
  const match = contents.match(FRONT_MATTER_RE);

  if (!match) {
    return null;
  }

  return {
    date: extractField(match[1], "date"),
    status: extractField(match[1], "status"),
  };
}

export async function setLearningStatus(filePath: string, status: string): Promise<boolean> {
  const contents = await readFile(filePath, "utf-8");
  const match = contents.match(FRONT_MATTER_RE);

  if (!match) {
    return false;
  }

  const currentFrontMatter = match[1];
  const nextFrontMatter = currentFrontMatter.replace(/^status:\s*.+$/m, `status: ${status}`);

  if (nextFrontMatter === currentFrontMatter) {
    return false;
  }

  const nextContents = contents.replace(FRONT_MATTER_RE, `---\n${nextFrontMatter}\n---\n`);
  if (nextContents === contents) {
    return false;
  }

  await writeFile(filePath, nextContents);
  return true;
}
