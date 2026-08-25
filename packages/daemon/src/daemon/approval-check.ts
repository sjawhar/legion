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

type ApprovalState = "success" | "pending";

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

export async function setApprovalStatus(
  effect: { repo: string; pr: number; sha: string },
  deps: ApprovalCheckDeps
): Promise<void> {
  if (deps.gatesMerge === "off") {
    return;
  }

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
  const state = computeApprovalState(
    parsedReviews.map((review) => ({
      author: review.user.login,
      state: review.state,
      commitId: review.commit_id,
    })),
    effect.sha,
    deps.appLogins
  );
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
  if (statusResult.exitCode !== 0) {
    throw new Error(`GitHub status write failed: ${statusResult.stderr || statusResult.stdout}`);
  }
}
