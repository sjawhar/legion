import { z } from "zod";
import type { CommandRunner } from "../state/fetch";
import type { GitHubAppRole } from "./config";
import { buildRoleEnv, type TokenManager } from "./github-apps";

const reviewsResponse = z.array(
  z.object({
    user: z.object({ login: z.string() }),
    state: z.string(),
    commit_id: z.string(),
  })
);

const SUCCESS_DESCRIPTION = "Approved by a human on the current head";
const PENDING_DESCRIPTION = "Awaiting human approval on the current head";

/** `gh api`'s own failure text on a non-2xx response, e.g. `gh: Resource not accessible by
 * integration (HTTP 403)`. There is no structured exit code per HTTP status, so this is the only
 * way to recover the status GitHub actually returned. */
const HTTP_STATUS_PATTERN = /\(HTTP (\d{3})\)/;
/** Non-2xx codes a retry can never fix on its own: the App's installation is missing the
 * `statuses: write` permission (401/403), the sha or repo no longer exists (404), or the request
 * itself is malformed (422). Every other failure (a 5xx, a network error, an unrecognized code)
 * is presumed transient and worth retrying as-is. */
const PERMANENT_STATUS_CODES: Record<number, true> = { 401: true, 403: true, 404: true, 422: true };
const APPROVAL_STATUS_REMEDY =
  "grant the App `Commit statuses: Read and write` and accept the installation permission update";

/** The result of one `setApprovalStatus` attempt: `written` on a 2xx response, otherwise
 * `permanent` classifies whether a retry can ever succeed without human intervention (see
 * `PERMANENT_STATUS_CODES`) and `reason` is a single, fully-composed message -- repo, sha, HTTP
 * status, and (for a permanent failure) the exact remedy -- ready for the caller to log verbatim
 * without assembling anything itself. */
export type SetApprovalStatusOutcome =
  | { written: true }
  | { written: false; permanent: boolean; reason: string };

export type ApprovalState = "success" | "pending";

type TokenLease = {
  token: string;
  expiresAt: string;
  gitIdentity: { name: string; email: string };
};

export interface ApprovalCheckDeps {
  runner: CommandRunner;
  tokenManager:
    | Pick<TokenManager, "getToken">
    | {
        getToken(role: GitHubAppRole, owner: string): Promise<TokenLease>;
      };
  appLogins: string[];
  gatesMerge: "human" | "off";
}

export function computeApprovalState(
  reviews: Array<{ author: string; state: string; commitId: string }>,
  headSha: string,
  appLogins: string[]
): ApprovalState {
  return reviews.some(
    (review) =>
      review.state === "APPROVED" &&
      review.commitId === headSha &&
      !appLogins.some((appLogin) => appLogin.toLowerCase() === review.author.toLowerCase())
  )
    ? "success"
    : "pending";
}

async function queryApprovalState(
  effect: { repo: string; pr: number; sha: string },
  deps: ApprovalCheckDeps
): Promise<{ state: ApprovalState; env: NodeJS.ProcessEnv }> {
  const [owner, repository, ...extra] = effect.repo.split("/");
  if (!owner || !repository || extra.length > 0) {
    throw new Error(`Invalid repository: ${effect.repo}`);
  }
  const lease = await deps.tokenManager.getToken("implement", owner);
  const env = buildRoleEnv(lease.token, lease.gitIdentity, process.env);
  const reviewsResult = await deps.runner(
    ["gh", "api", `repos/${effect.repo}/pulls/${effect.pr}/reviews`],
    {
      env,
    }
  );
  if (reviewsResult.exitCode !== 0) {
    throw new Error(
      `GitHub reviews request failed: ${reviewsResult.stderr || reviewsResult.stdout}`
    );
  }
  const parsedReviews = reviewsResponse.parse(JSON.parse(reviewsResult.stdout));
  return {
    state: computeApprovalState(
      parsedReviews.map((review) => ({
        author: review.user.login,
        state: review.state,
        commitId: review.commit_id,
      })),
      effect.sha,
      deps.appLogins
    ),
    env,
  };
}

export async function getApprovalState(
  effect: { repo: string; pr: number; sha: string },
  deps: ApprovalCheckDeps
): Promise<ApprovalState> {
  return (await queryApprovalState(effect, deps)).state;
}

/** Writes the human-approval backstop status for `effect`'s head sha. Never throws for an HTTP
 * failure -- only for a programmer error (a malformed `effect.repo`, surfaced by
 * `queryApprovalState`'s own validation): a non-2xx response from the POST is classified instead
 * and handed back as a `SetApprovalStatusOutcome` for the caller (`events.ts`'s durable dispatch)
 * to record and retry. This backstop status is never the merge gate itself (a human `APPROVED`
 * PR review is), so a write that can never succeed must not take the daemon down. */
export async function setApprovalStatus(
  effect: { repo: string; pr: number; sha: string },
  deps: ApprovalCheckDeps
): Promise<SetApprovalStatusOutcome> {
  if (deps.gatesMerge === "off") {
    return { written: true };
  }

  const { state, env } = await queryApprovalState(effect, deps);
  const statusResult = await deps.runner(
    [
      "gh",
      "api",
      "-X",
      "POST",
      `repos/${effect.repo}/statuses/${effect.sha}`,
      "-f",
      "context=legion-human-approval",
      "-f",
      `state=${state}`,
      "-f",
      `description=${state === "success" ? SUCCESS_DESCRIPTION : PENDING_DESCRIPTION}`,
    ],
    { env }
  );
  if (statusResult.exitCode === 0) {
    return { written: true };
  }

  const detail = (statusResult.stderr || statusResult.stdout).trim();
  const code = Number(detail.match(HTTP_STATUS_PATTERN)?.[1]);
  const permanent = Number.isInteger(code) && PERMANENT_STATUS_CODES[code] === true;
  const reason = permanent
    ? `GitHub status write to ${effect.repo}@${effect.sha} failed permanently (HTTP ${code}): ${detail} -- ${APPROVAL_STATUS_REMEDY}.`
    : `GitHub status write to ${effect.repo}@${effect.sha} failed${Number.isInteger(code) ? ` (HTTP ${code})` : ""}: ${detail}`;
  return { written: false, permanent, reason };
}
