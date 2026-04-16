import {
  createParsedIssue,
  GitHubPRRef,
  type GitHubPRRef as GitHubPRRefType,
  type IssueSource,
  IssueStatus,
  type IssueStatusLiteral,
  type ParsedIssue,
} from "../types";
import type { IssueMutationTarget, IssueTracker } from "./issue-tracker";

interface GitHubProjectItem {
  id?: string;
  content?: {
    number?: number;
    repository?: string;
    url?: string;
    type?: string;
  };
  status?: string | null;
  labels?: string[] | null;
  "linked pull requests"?: string[] | null;
  isBlocked?: boolean;
  blockerRefs?: Array<{ number: number; repository: string }>;
  [key: string]: unknown;
}

const LINKED_PR_PATTERN = /^linked\s*pull\s*requests?$/i;

function extractItems(raw: unknown): unknown[] {
  if (Array.isArray(raw)) {
    return raw;
  }
  if (typeof raw === "object" && raw !== null && "items" in raw) {
    const wrapped = raw as { items?: unknown };
    if (Array.isArray(wrapped.items)) {
      return wrapped.items;
    }
  }
  return [];
}

function parseOwnerRepo(repository: string): { owner: string; repo: string } | null {
  const parts = repository.split("/");
  if (parts.length !== 2 || !parts[0] || !parts[1]) {
    return null;
  }
  return { owner: parts[0], repo: parts[1] };
}

// Human-readable key and worker-ID component. Ambiguous for repos with
// hyphens (e.g. "acme-my-widgets-42" could be acme/my-widgets or acme-my/widgets).
// Use ParsedIssue.source for API calls — it carries the canonical owner/repo/number.
function buildIssueId(owner: string, repo: string, number: number): string {
  const safeOwner = owner.toLowerCase().replace(/[^a-z0-9-]/g, "-");
  const safeRepo = repo.toLowerCase().replace(/[^a-z0-9-]/g, "-");
  return `${safeOwner}-${safeRepo}-${number}`;
}

/**
 * Parse an issueId back into owner/repo/number components.
 *
 * IssueId format: `{owner}-{repo}-{number}` (lowercase, hyphens).
 * Ambiguous when owner or repo contains hyphens — prefer IssueSource
 * from cached state for API calls.
 */
export function parseIssueIdParts(issueId: string): {
  owner: string;
  repo: string;
  number: string;
} {
  const parts = issueId.split("-");
  // Find the last all-numeric segment (the issue number)
  let numberIdx = -1;
  for (let i = parts.length - 1; i >= 0; i--) {
    if (/^\d+$/.test(parts[i])) {
      numberIdx = i;
      break;
    }
  }
  if (numberIdx < 2) {
    throw new Error(`Cannot parse issueId "${issueId}" — expected format: owner-repo-number`);
  }
  // Convention: first segment is owner, middle segments are repo, last numeric is number
  const owner = parts[0];
  const repo = parts.slice(1, numberIdx).join("-");
  const number = parts[numberIdx];
  return { owner, repo, number };
}

export async function runGhCommand(args: string[]): Promise<string> {
  const proc = Bun.spawn(["gh", ...args], { stdout: "pipe", stderr: "pipe" });
  const killTimeout = setTimeout(() => {
    try {
      proc.kill();
    } catch {
      // Process may have already exited
    }
  }, 30_000);
  const [stdout, stderr] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
  ]);
  const exitCode = await proc.exited;
  clearTimeout(killTimeout);
  if (exitCode !== 0) {
    throw new Error(`gh ${args[0]} failed (exit ${exitCode}): ${stderr.trim()}`);
  }
  return stdout;
}

async function runGraphQL(query: string): Promise<Record<string, unknown>> {
  const stdout = await runGhCommand(["api", "graphql", "-f", `query=${query}`]);
  return JSON.parse(stdout);
}

export class GitHubTracker implements IssueTracker {
  parseIssues(raw: unknown): ParsedIssue[] {
    const items = extractItems(raw);
    const parsed: ParsedIssue[] = [];

    for (const item of items) {
      if (typeof item !== "object" || item === null) {
        continue;
      }
      const typedItem = item as GitHubProjectItem;
      const content = typedItem.content;
      if (!content || content.type !== "Issue") {
        continue;
      }

      const number = content.number;
      const repository = content.repository;
      if (typeof number !== "number" || typeof repository !== "string") {
        continue;
      }

      const ownerRepo = parseOwnerRepo(repository);
      if (!ownerRepo) {
        continue;
      }

      const issueId = buildIssueId(ownerRepo.owner, ownerRepo.repo, number);
      const status = IssueStatus.normalize(typedItem.status ?? null);

      let labels: string[] = [];
      if (Array.isArray(typedItem.labels)) {
        labels = typedItem.labels.filter(
          (label): label is string => typeof label === "string" && label !== ""
        );
      }

      let prRef: GitHubPRRefType | null = null;
      let linkedPRs: unknown = typedItem["linked pull requests"];
      if (!Array.isArray(linkedPRs)) {
        for (const key of Object.keys(typedItem)) {
          if (LINKED_PR_PATTERN.test(key)) {
            linkedPRs = typedItem[key];
            break;
          }
        }
      }
      if (Array.isArray(linkedPRs)) {
        for (const prUrl of linkedPRs) {
          if (typeof prUrl === "string") {
            prRef = GitHubPRRef.fromUrl(prUrl);
            if (prRef) break;
          }
        }
      }

      const source: IssueSource = {
        owner: ownerRepo.owner,
        repo: ownerRepo.repo,
        number,
        url: typeof content.url === "string" ? content.url : "",
      };
      // Build blockedByIds from raw blocker references
      const blockedByIds: string[] = [];
      if (Array.isArray(typedItem.blockerRefs)) {
        for (const ref of typedItem.blockerRefs) {
          if (typeof ref.number === "number" && typeof ref.repository === "string") {
            const blockerOwnerRepo = parseOwnerRepo(ref.repository);
            if (blockerOwnerRepo) {
              blockedByIds.push(
                buildIssueId(blockerOwnerRepo.owner, blockerOwnerRepo.repo, ref.number)
              );
            }
          }
        }
      }
      // Dedup and sort for stable output
      const uniqueBlockedByIds = [...new Set(blockedByIds)].sort();

      parsed.push(createParsedIssue(issueId, status, labels, prRef, source, uniqueBlockedByIds));
    }

    return parsed;
  }

  /**
   * Resolve owner/repo/number from an IssueMutationTarget.
   * Prefers source (canonical) over issueId parsing (lossy for hyphenated owners).
   */
  private resolveTarget(target: IssueMutationTarget): {
    owner: string;
    repo: string;
    number: string;
  } {
    if (target.source) {
      return {
        owner: target.source.owner,
        repo: target.source.repo,
        number: String(target.source.number),
      };
    }
    return parseIssueIdParts(target.issueId);
  }

  async removeLabel(target: IssueMutationTarget, label: string): Promise<void> {
    const { owner, repo, number } = this.resolveTarget(target);
    await runGhCommand([
      "issue",
      "edit",
      number,
      "--remove-label",
      label,
      "-R",
      `${owner}/${repo}`,
    ]);
  }

  async transitionIssue(target: IssueMutationTarget, newStatus: IssueStatusLiteral): Promise<void> {
    const { owner, repo, number } = this.resolveTarget(target);
    const issueNum = Number(number);

    // Step 1: Query the project item ID, project ID, Status field ID, and option IDs
    const queryResult = await runGraphQL(`{
      repository(owner: "${owner}", name: "${repo}") {
        issue(number: ${issueNum}) {
          projectItems(first: 10) {
            nodes {
              id
              project {
                id
                field(name: "Status") {
                  ... on ProjectV2SingleSelectField {
                    id
                    options { id name }
                  }
                }
              }
            }
          }
        }
      }
    }`);

    const data = queryResult?.data as Record<string, unknown> | undefined;
    const repository = data?.repository as Record<string, unknown> | undefined;
    const issue = repository?.issue as Record<string, unknown> | undefined;
    const projectItems = issue?.projectItems as Record<string, unknown> | undefined;
    const nodes = projectItems?.nodes as Array<Record<string, unknown>> | undefined;

    if (!nodes || nodes.length === 0) {
      throw new Error(`transitionIssue: ${target.issueId} has no project items`);
    }

    // Use the first project item that has a Status field
    const item = nodes.find((n) => {
      const project = n.project as Record<string, unknown> | undefined;
      return project?.field != null;
    });
    if (!item) {
      throw new Error(`transitionIssue: ${target.issueId} has no project with Status field`);
    }

    const project = item.project as Record<string, unknown>;
    const projectId = project.id as string;
    const field = project.field as Record<string, unknown>;
    const fieldId = field.id as string;
    const options = field.options as Array<{ id: string; name: string }>;
    const targetOption = options.find((o) => o.name === newStatus);
    if (!targetOption) {
      const available = options.map((o) => o.name).join(", ");
      throw new Error(
        `transitionIssue: status option "${newStatus}" not found for ${target.issueId}. Available: ${available}`
      );
    }

    // Step 2: Mutation to update the status
    await runGraphQL(`mutation {
      updateProjectV2ItemFieldValue(input: {
        projectId: "${projectId}"
        itemId: "${item.id}"
        fieldId: "${fieldId}"
        value: { singleSelectOptionId: "${targetOption.id}" }
      }) {
        projectV2Item { id }
      }
    }`);
  }
}
