import { readdir, realpath } from "node:fs/promises";
import path from "node:path";
import { loadSkills } from "@oh-my-pi/pi-coding-agent";
import { discoverAgents } from "@oh-my-pi/pi-coding-agent/task/discovery";

const DEPENDENCIES_VARIABLE = "LEGION_PROMPT_DEPENDENCIES";
const RESOLVED_MARKER = "LEGION_OMP_PROMPT_DEPENDENCIES=resolved";
const MISSING_MARKER = "LEGION_OMP_PROMPT_DEPENDENCIES=missing:";

interface PromptDependencies {
  readonly agents: readonly string[];
  readonly skills: readonly string[];
}

function stringList(value: unknown, name: string): readonly string[] {
  if (!Array.isArray(value) || value.some((entry) => typeof entry !== "string")) {
    throw new Error(`${DEPENDENCIES_VARIABLE}.${name} must be an array of strings`);
  }
  return value;
}

function readDependencies(): PromptDependencies {
  const raw = process.env[DEPENDENCIES_VARIABLE];
  if (raw === undefined) throw new Error(`${DEPENDENCIES_VARIABLE} is required`);
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    throw new Error(`${DEPENDENCIES_VARIABLE} must be valid JSON`);
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    throw new Error(`${DEPENDENCIES_VARIABLE} must be an object`);
  }
  const record = parsed as Record<string, unknown>;
  return {
    agents: stringList(record.agents, "agents"),
    skills: stringList(record.skills, "skills"),
  };
}

async function isLegionAgent(filePath: string | undefined, directory: string): Promise<boolean> {
  if (filePath === undefined) return false;
  try {
    return (await realpath(filePath)).startsWith(`${directory}${path.sep}`);
  } catch {
    return false;
  }
}

export default async function probeLegionPromptDependencies(): Promise<void> {
  const expected = readDependencies();
  const legionAgentsDirectory = await realpath(path.resolve(import.meta.dir, "../agents"));
  const legionAgentNames = new Set(
    (await readdir(legionAgentsDirectory))
      .filter((entry) => entry.endsWith(".md"))
      .map((entry) => path.basename(entry, ".md"))
  );
  const [{ agents }, { skills }] = await Promise.all([
    discoverAgents(process.cwd()),
    loadSkills({ cwd: process.cwd() }),
  ]);
  const agentNames = new Set(agents.map((agent) => agent.name));
  const skillNames = new Set(skills.map((skill) => skill.name));
  const agentSources = await Promise.all(
    expected.agents.map(async (name) => {
      const agent = agents.find((candidate) => candidate.name === name);
      if (agent === undefined || !legionAgentNames.has(name)) return undefined;
      return (await isLegionAgent(agent.filePath, legionAgentsDirectory)) ? undefined : name;
    })
  );
  const missing = [
    ...expected.agents.filter((name) => !agentNames.has(name)).map((name) => `agent:${name}`),
    ...agentSources
      .filter((name): name is string => name !== undefined)
      .map((name) => `agent-source:${name}`),
    ...expected.skills.filter((name) => !skillNames.has(name)).map((name) => `skill:${name}`),
  ];
  process.stderr.write(
    missing.length === 0 ? `${RESOLVED_MARKER}\n` : `${MISSING_MARKER}${missing.join(",")}\n`
  );
}
