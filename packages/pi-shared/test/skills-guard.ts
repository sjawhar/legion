// The rules each plugin's skills-guard.test.ts runs over the skills it stages into dist/skills
// (scripts/pi-plugin-prepack.sh's partition for its package name). The skills are read by agents,
// and how much of each reaches the model is decided by Oh My Pi: a tool result over 51,200 bytes
// (`tools.artifactSpillThreshold`, 50 KiB) spills to an artifact that keeps only 20 KB at each end,
// so a longer skill arrives with its middle cut out. A `skill://` read is otherwise whole (no line
// limit, no line-length cap), so the file's own byte count is the measure. Reads by filesystem path
// stop at 300 lines, which is why every reference is linked as `skill://<name>/<path>`, and why each
// such link must resolve (the daemon's boot gate checks only the name before the first `/`).
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { linkOmpNatives } from "./omp-natives";

export const SPILL_THRESHOLD_BYTES = 50 * 1024;
/** The repository root, where `skills/` and the prepack script live. */
export const REPO_ROOT = path.resolve(import.meta.dir, "../../..");
const PREPACK_SCRIPT = path.join(REPO_ROOT, "scripts/pi-plugin-prepack.sh");
// A link's path stops at whitespace, a closing bracket or quote, a code span, or Markdown emphasis
// (`**skill://…/pr-body.md**`); `#anchor` is kept so it can be checked against the target's headings.
const SKILL_LINK = /skill:\/\/([a-z0-9](?:[a-z0-9._-]*[a-z0-9])?)\/([^\s)`'"\]>*]+)/g;
// A relative Markdown link, `](../…)` or `](./…)`, up to its closing parenthesis.
const RELATIVE_LINK = /\]\((\.\.?\/[^\s)]+)\)/g;
// A task dispatch as the daemon's boot gate reads it (packages/daemon/internal/promptrefs/
// promptrefs.go): the name inside `agent="…"`, as `task(agent="<name>")` writes it.
const AGENT_DISPATCH = /agent="([a-z0-9][a-z0-9._-]*)"/g;

/**
 * Stages `packageName`'s skill partition into a fresh temporary directory through the prepack
 * script's `--stage-skills` mode, exactly as its `prepack` stages dist/skills, and returns it. The
 * caller removes the directory.
 */
export function stageSkills(packageName: string): string {
  const dest = path.join(mkdtempSync(path.join(os.tmpdir(), "staged-skills-")), "skills");
  const staged = Bun.spawnSync(["bash", PREPACK_SCRIPT, "--stage-skills", packageName, dest]);
  if (staged.exitCode !== 0) {
    rmSync(path.dirname(dest), { recursive: true, force: true });
    throw new Error(
      `${PREPACK_SCRIPT} --stage-skills ${packageName} exited ${staged.exitCode}:\n${staged.stderr.toString()}`
    );
  }
  return dest;
}

/** Every file under `directory`, recursively. */
export function files(directory: string): string[] {
  return readdirSync(directory, { recursive: true, encoding: "utf8" })
    .map((entry) => path.join(directory, entry))
    .filter((file) => statSync(file).isFile());
}

/** Every `<skillsRoot>/<name>/SKILL.md`. */
export function skillFiles(skillsRoot: string): string[] {
  return readdirSync(skillsRoot)
    .map((name) => path.join(skillsRoot, name, "SKILL.md"))
    .filter((file) => existsSync(file));
}

/**
 * The lines outside fenced code blocks, where a Markdown reader finds headings: a fence opens on
 * three or more backticks or tildes (up to three spaces in), closes on a line holding only a run of
 * the same character at least as long, and an unclosed fence runs to the end of the file.
 */
export function outsideFences(markdown: string): string[] {
  const kept: string[] = [];
  let fence: string | undefined;
  for (const line of markdown.split("\n")) {
    const marker = /^ {0,3}(`{3,}|~{3,})/.exec(line)?.[1];
    if (fence === undefined) {
      if (marker === undefined) kept.push(line);
      else fence = marker;
    } else if (
      marker?.[0] === fence[0] &&
      marker.length >= fence.length &&
      line.trim() === marker
    ) {
      fence = undefined;
    }
  }
  return kept;
}

/** GitHub's heading anchor: lower-cased, punctuation dropped, each space a hyphen. */
export function anchors(markdown: string): Set<string> {
  return new Set(
    outsideFences(markdown).flatMap((line) => {
      const heading = /^#{1,6}\s+(.+?)\s*#*$/.exec(line)?.[1];
      if (heading === undefined) return [];
      return [
        heading
          .toLowerCase()
          .replace(/[^\p{L}\p{N}\s_-]/gu, "")
          .replace(/\s/g, "-"),
      ];
    })
  );
}

export interface SkillLink {
  /** The Markdown file holding the link. */
  readonly source: string;
  readonly name: string;
  readonly target: string;
  readonly anchor: string | undefined;
}

/** Every `skill://<name>/<path>[#anchor]` link in the Markdown files under `linkingRoots`. */
export function skillLinks(linkingRoots: readonly string[]): SkillLink[] {
  return linkingRoots
    .flatMap(files)
    .filter((file) => file.endsWith(".md"))
    .flatMap((file) =>
      [...readFileSync(file, "utf8").matchAll(SKILL_LINK)].map(([, name = "", raw]) => {
        // A link that ends a sentence carries its full stop.
        const [target = "", anchor] = (raw ?? "").replace(/[.,;:]+$/, "").split("#");
        return { source: file, name, target, anchor };
      })
    );
}

/** The files under `root` at or over Oh My Pi's spill threshold, so a skill:// read of one is cut. */
export function oversizedFiles(root: string): { file: string; bytes: number }[] {
  return files(root)
    .map((file) => ({ file: path.relative(root, file), bytes: statSync(file).size }))
    .filter(({ bytes }) => bytes >= SPILL_THRESHOLD_BYTES);
}

/** The skill bodies under `skillsRoot` of 500 lines or more, whose detail belongs in references. */
export function longSkillBodies(skillsRoot: string): { file: string; lines: number }[] {
  return skillFiles(skillsRoot)
    .map((file) => ({
      file: path.relative(skillsRoot, file),
      lines: readFileSync(file, "utf8").split("\n").length,
    }))
    .filter(({ lines }) => lines >= 500);
}

/**
 * The skills under `skillsRoot` whose frontmatter name is not their directory's name. Oh My Pi
 * resolves `skill://<name>` by the frontmatter name, and `brokenSkillLinks` resolves it by the
 * directory, so the two must agree.
 */
export function misnamedSkills(
  skillsRoot: string
): { directory: string; name: string | undefined }[] {
  return skillFiles(skillsRoot)
    .map((file) => {
      const frontmatter = /^---\n([\s\S]*?)\n---\n/.exec(readFileSync(file, "utf8"))?.[1] ?? "";
      return {
        directory: path.basename(path.dirname(file)),
        name: /^name:\s*(.*?)\s*$/m.exec(frontmatter)?.[1],
      };
    })
    .filter(({ directory, name }) => name !== directory);
}

/**
 * The links of `links` that name no file under `<skillsRoot>/<name>/`, or an anchor no heading of
 * that file renders, each as one sentence naming the source.
 */
export function brokenSkillLinks(links: readonly SkillLink[], skillsRoot: string): string[] {
  return links.flatMap(({ source, name, target, anchor }) => {
    const link = `skill://${name}/${target}`;
    const file = path.join(skillsRoot, name, target);
    if (!existsSync(file)) return [`${source}: ${link} names no file`];
    if (anchor !== undefined && !anchors(readFileSync(file, "utf8")).has(anchor)) {
      return [`${source}: ${link}#${anchor} names no heading`];
    }
    return [];
  });
}

/**
 * The relative Markdown links (`](../…)`, `](./…)`) in the files under `partitionRoot` whose target
 * is not a file inside it. A staged partition is installed on its own, so a link out of it, into a
 * skill the other plugin ships, dangles there; such a link is written `skill://<name>/<path>`,
 * which resolves by name once both plugins are installed. Links inside code fences are examples,
 * not links a reader follows, so they are not held to this.
 */
export function brokenRelativeLinks(partitionRoot: string): string[] {
  const inside = `${path.resolve(partitionRoot)}${path.sep}`;
  return files(partitionRoot)
    .filter((file) => file.endsWith(".md"))
    .flatMap((file) =>
      [...outsideFences(readFileSync(file, "utf8")).join("\n").matchAll(RELATIVE_LINK)].flatMap(
        ([, raw = ""]) => {
          const target = raw.split("#")[0] ?? "";
          const resolved = path.resolve(path.dirname(file), target);
          if (!resolved.startsWith(inside)) {
            return [`${path.relative(partitionRoot, file)}: ${raw} leaves the staged partition`];
          }
          if (!existsSync(resolved) || !statSync(resolved).isFile()) {
            return [`${path.relative(partitionRoot, file)}: ${raw} names no file`];
          }
          return [];
        }
      )
    );
}

export interface AgentDispatch {
  /** The Markdown file holding the dispatch. */
  readonly source: string;
  readonly name: string;
}

/**
 * Every `task(agent="<name>")` dispatch in the Markdown files under `roots`. Code fences are not
 * excluded: the daemon's gate reads a prompt whole, and a dispatch shown in a fence is still what a
 * model copies.
 */
export function agentDispatches(roots: readonly string[]): AgentDispatch[] {
  return roots
    .flatMap(files)
    .filter((file) => file.endsWith(".md"))
    .flatMap((file) =>
      [...readFileSync(file, "utf8").matchAll(AGENT_DISPATCH)].map(([, name = ""]) => ({
        source: file,
        name,
      }))
    );
}

/**
 * The task agents the Oh My Pi binary `omp` resolves with no plugin installed: the names of the
 * files `omp agents unpack` writes under a fresh HOME. That HOME is a temporary directory holding
 * only the binary's natives (hardlinks to the shared copy, omp-natives.ts), and the binary's
 * environment names no profile or config directory, so it writes where a person's does. The
 * directory is removed before returning.
 */
export async function bundledAgents(omp: string): Promise<string[]> {
  const home = mkdtempSync(path.join(os.tmpdir(), "omp-bundled-agents-"));
  try {
    await linkOmpNatives(omp, home);
    const unpacked = Bun.spawnSync([omp, "agents", "unpack"], {
      env: { HOME: home, PATH: "/usr/local/bin:/usr/bin:/bin" },
    });
    if (unpacked.exitCode !== 0) {
      throw new Error(
        `${omp} agents unpack exited ${unpacked.exitCode}:\n${unpacked.stderr.toString()}`
      );
    }
    return files(path.join(home, ".omp"))
      .filter((file) => file.endsWith(".md"))
      .map((file) => path.basename(file, ".md"));
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
}

/** The task agents the plugin at `packageRoot` ships: the `*.md` under its `agents/`, if any. */
export function shippedAgents(packageRoot: string): string[] {
  const agents = path.join(packageRoot, "agents");
  if (!existsSync(agents)) return [];
  return readdirSync(agents)
    .filter((file) => file.endsWith(".md"))
    .map((file) => path.basename(file, ".md"));
}

/**
 * The dispatches in the Markdown files under `roots` naming an agent not in `available`, each as
 * one sentence naming the source.
 */
export function unresolvedAgentDispatches(
  roots: readonly string[],
  available: ReadonlySet<string>
): string[] {
  return agentDispatches(roots)
    .filter(({ name }) => !available.has(name))
    .map(
      ({ source, name }) =>
        `${source} dispatches ${name}, which neither this plugin nor Oh My Pi ships`
    );
}
