import type {
  Comparison,
  ComparisonSide,
  FixCounts,
  MergeSection,
  Summary,
  Tables,
  Tier,
} from "./summary.ts";

const instant = (ms: number) => new Date(ms).toISOString().replace(".000Z", "Z");
const percent = (share: number) => `${(100 * share).toFixed(1)}%`;

/** Python's repr of a string, as its dict repr prints the force-push error counter. */
function pyRepr(value: string | null): string {
  if (value === null) return "None";
  const quote = value.includes("'") && !value.includes('"') ? '"' : "'";
  return `${quote}${value.replaceAll("\\", "\\\\").replaceAll(quote, `\\${quote}`)}${quote}`;
}

function tierLines(tiers: Tier[]): string[] {
  return tiers.map(
    (tier) => `  - ${tier.name}: ${tier.hit} of ${tier.total} events (${tier.prs} PRs)`
  );
}

const fixCells = (fix: FixCounts | null | undefined) =>
  fix ? ` ${fix.before} | ${fix.transition} | ${fix.after} |` : "  |  |  |";

function tableLines(tables: Tables, title: string, fixes = false): string[] {
  const lines = [
    `### ${title}: by category (${tables.events} events; an event counts once per category it touches)`,
    "| category | events | PRs |\n|---|---:|---:|",
    ...tables.categories.map((row) => `| ${row.category} | ${row.events} | ${row.prs} |`),
    `\n### ${title}: by file`,
    fixes
      ? "| file | category | events | PRs | before | transition | after | PR numbers |\n" +
        "|---|---|---:|---:|---:|---:|---:|---|"
      : "| file | category | events | PRs | PR numbers |\n|---|---|---:|---:|---|",
  ];
  for (const row of tables.files) {
    const numbers = row.prs.map((pr) => `#${pr}`).join(", ");
    const counts = `| ${row.path} | ${row.category} | ${row.events} | ${row.prs.length} |`;
    lines.push(`${counts}${fixes ? fixCells(row.fix) : ""} ${numbers} |`);
  }
  lines.push("");
  return lines;
}

function mergeLines(section: MergeSection, label: string): string[] {
  const perPr = section.perPr.map(({ pr, conflicting }) => `#${pr}:${conflicting}`).join(", ");
  return [
    `## Forward merges (${label})`,
    `- PRs: ${section.prs}; PRs carrying >=1 forward merge: ${section.carryingForward}`,
    `- unique forward-merge commits: ${section.forward}; conflicting: ${section.conflicting} ` +
      `(carried by ${section.conflictingPrs} PRs); clean, no conflict: ${section.clean} ` +
      `(${section.cleanPrs} PRs); undeterminable: ${section.undeterminable}`,
    "- conflicting merges whose every conflicted path is in:",
    ...tierLines(section.tiers),
    `- merges of a branch neither main nor the base: ${section.otherBranch}; merges between two ` +
      `commits of the PR (e.g. stacked branch into branch): ${section.internal}`,
    `- conflicting merges per PR: ${perPr}\n`,
  ];
}

function combinedLines(summary: Summary): string[] {
  const { combined, fixes } = summary;
  const lines = [
    "## Conflicting forward merges, all PRs read (merged and other together; a merge counts once)",
    `- PRs read: ${combined.prs}; PRs carrying >=1 conflicting forward merge: ${combined.conflictingPrs}`,
    `- conflicting forward merges: ${combined.conflicting}; clean, no conflict: ${combined.clean} ` +
      `(${combined.cleanPrs} PRs); undeterminable: ${combined.undeterminable}`,
    "- conflicting merges whose every conflicted path is in:",
    ...tierLines(combined.tiers),
  ];
  for (const fix of fixes ?? []) {
    const states = fix.states
      ? `; before ${fix.states.before}, transition ${fix.states.transition}, ` +
        `after ${fix.states.after}`
      : "";
    lines.push(
      `- conflicting merges on files matching \`${fix.glob}\`, fixed by ` +
        `${fix.commit.slice(0, 12)}: ${fix.merges} (${fix.prs} PRs)${states}`
    );
  }
  lines.push("");
  const withStates = fixes?.some((fix) => fix.states !== null) ?? false;
  return [
    ...lines,
    ...tableLines(combined.tables, "Conflicting forward merges, all PRs read", withStates),
  ];
}

/** The report: the combined section first, then Python's sections and table columns. */
export function renderSummary(summary: Summary): string {
  const { window, merged, other, forcePushes, snapshot } = summary;
  const lines = [
    `# Conflicts: ${window.since}..${window.until} (${merged.prs + other.prs} PRs read: ` +
      `${merged.prs} merged in the window, ${other.prs} other)` +
      (summary.prefix ? `, head branches starting \`${summary.prefix}\`` : ""),
    `origin/main = ${summary.originMain}; merges and force pushes counted only when dated ` +
      `${instant(window.start)}..${instant(window.end)}` +
      (window.measuredAt ? `; measured ${window.measuredAt}` : "") +
      "\n",
    ...combinedLines(summary),
    ...mergeLines(merged, "merged in the window"),
    ...tableLines(merged.tables, "Conflicting forward merges, merged PRs"),
    ...mergeLines(
      other,
      "every other pull request read; merges already counted under merged in the window excluded"
    ),
  ];
  if (other.conflicting > 0) {
    lines.push(...tableLines(other.tables, "Conflicting forward merges, other PRs read"));
  }

  const errors = forcePushes.errors.map(({ error, count }) => `${pyRepr(error)}: ${count}`);
  lines.push(
    "## Force pushes (every PR read)",
    `- force pushes: ${forcePushes.count} in ${forcePushes.prs} PRs; pre-push head conflicted ` +
      `with main at push time: ${forcePushes.conflicting} in ${forcePushes.conflictingPrs} PRs; ` +
      `undeterminable: ${forcePushes.undeterminable} ({${errors.join(", ")}})`
  );
  if (forcePushes.conflicting > 0) {
    lines.push(
      "- conflicting force pushes whose every conflicted path is in:",
      ...tierLines(forcePushes.tiers)
    );
  }
  lines.push("");
  if (forcePushes.conflicting > 0) {
    lines.push(...tableLines(forcePushes.tables, "Force pushes whose pre-push head conflicted"));
  }

  if (snapshot.open > 0) {
    lines.push(
      "## Open-PR snapshot: head merged with the current tip of its base branch",
      `- open PRs: ${snapshot.open}; conflicting now: ${snapshot.conflicting}; of those, every ` +
        "conflicted path is in:",
      ...tierLines(snapshot.tiers),
      "",
      ...tableLines(snapshot.tables, "Open PRs conflicting now"),
      "| PR | base | conflicted files |\n|---|---|---|",
      ...snapshot.rows.map((row) => `| #${row.pr} | ${row.base} | ${row.conflicts.join(", ")} |`),
      ""
    );
  }
  return lines.join("\n");
}

/** Before → after over the two runs' combined sections, as markdown tables. */
export function renderComparison(c: Comparison): string {
  const sides = [c.before, c.after];
  const row = (label: string, cell: (side: ComparisonSide) => string) =>
    `| ${label} | ${sides.map(cell).join(" | ")} |`;
  const lines = [
    "# Conflicts before and after",
    "",
    "| | before | after |\n|---|---:|---:|",
    row("window given", ({ window }) => `${window.since}..${window.until}`),
    row("window counted", ({ window }) => `${instant(window.start)}..${instant(window.end)}`),
    row("length (days)", ({ window }) => window.days.toFixed(2)),
    row("PRs read", (side) => `${side.prsRead}`),
    row(
      "PRs carrying >=1 conflicting forward merge (share of PRs read)",
      (side) => `${side.carrying} (${percent(side.carryingShare)})`
    ),
    row(
      "conflicting forward merges (per PR read)",
      (side) => `${side.conflicting} (${side.conflictingPerPr.toFixed(2)})`
    ),
    ...c.before.tiers.map((tier, index) =>
      row(
        `every conflicted path in: ${tier.name} (share of conflicting merges)`,
        (side) => `${side.tiers[index].events} (${percent(side.tiers[index].share)})`
      )
    ),
    "",
    "## Conflicting forward merges by file",
    "",
    c.fixes
      ? "| file | category | events before | PRs before | events after | PRs after " +
        "| after: before fix | after: transition | after: after fix |\n" +
        "|---|---|---:|---:|---:|---:|---:|---:|---:|"
      : "| file | category | events before | PRs before | events after | PRs after |\n" +
        "|---|---|---:|---:|---:|---:|",
  ];
  for (const file of c.files) {
    const counts =
      `| ${file.path} | ${file.category} | ${file.before.events} | ${file.before.prs} | ` +
      `${file.after.events} | ${file.after.prs} |`;
    lines.push(c.fixes ? counts + fixCells(file.after.fix) : counts);
  }
  lines.push("");
  if (c.globs.length > 0) {
    lines.push(
      "## Conflicting forward merges by fixed glob (a merge counts once per glob)",
      "",
      "| glob | merges before | PRs before (share of PRs read) | merges after " +
        "| PRs after (share of PRs read) | after: before fix | after: transition | after: after fix |\n" +
        "|---|---:|---:|---:|---:|---:|---:|---:|"
    );
    for (const { glob, before, after } of c.globs) {
      lines.push(
        `| \`${glob}\` | ${before.merges} | ${before.prs} (${percent(before.prsShare)}) | ` +
          `${after.merges} | ${after.prs} (${percent(after.prsShare)}) |${fixCells(after.states)}`
      );
    }
    lines.push("");
  }
  return lines.join("\n");
}
