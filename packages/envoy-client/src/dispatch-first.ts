import { readFileSync } from "node:fs";
import { join } from "node:path";
import { messageFor } from "./errors";

/**
 * Opens the dispatch-first skill as a host puts it into a session's context. Nothing else
 * carries it, so a request that already holds the injected skill is recognised by this tag
 * alone; the skill's own words would also match a `read skill://dispatch-first` result.
 */
export const DISPATCH_FIRST_MARKER = "<dispatch-first-skill>";

/** The skill's file under a plugin's staged `skills/` directory. */
export function dispatchFirstSkillFile(skillsDirectory: string): string {
  return join(skillsDirectory, "dispatch-first", "SKILL.md");
}

/**
 * The dispatch-first skill as Oh My Pi and Claude Code inject it into a session with Dispatch:
 * the file without its frontmatter, inside the marker tags. A missing or unreadable file throws
 * naming it, because a plugin packed without its skill is a build defect, never a session that
 * should quietly start without the rule.
 */
export function readDispatchFirstContext(skillFile: string): string {
  let skill: string;
  try {
    skill = readFileSync(skillFile, "utf8");
  } catch (error) {
    throw new Error(`the dispatch-first skill ${skillFile} could not be read: ${messageFor(error)}`);
  }
  const body = skill.replace(/^---\n[\s\S]*?\n---\n/, "").trim();
  return [
    DISPATCH_FIRST_MARKER,
    "This session has Dispatch, so the dispatch-first skill is loaded for it. Follow it.",
    "",
    body,
    "</dispatch-first-skill>",
  ].join("\n");
}
