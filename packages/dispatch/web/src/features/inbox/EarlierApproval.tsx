import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import type { ArtifactVersionContent, ArtifactVersionText, Ask } from "../../api/types";
import { DisclosureToggle } from "../../components/DisclosureToggle";
import { dangerText, textMutedOnSurface } from "../../theme/classes";
import { cutDecisionBlocks } from "../doc/decision-blocks";
import { VersionDiff } from "../doc/VersionDiff";
import { actorLabel } from "../refs/actor";

type AskApproval = NonNullable<Ask["approval"]>;

function isText(content: ArtifactVersionContent | undefined): content is ArtifactVersionText {
  return content !== undefined && "markdown" in content;
}

/**
 * The diff between the version a human approved and the version the request names, with every
 * decision block cut out of both (`cutDecisionBlocks`), so it shows what changed outside them:
 * the answers to decision blocks between the two are what the human himself decided. Mounted
 * only once the reader asks for it, so nothing is fetched or rendered before. Both reads share
 * the document page's `["artifact", id, "version", n]` keys, and the loading and error copy is
 * its comparison's (`features/artifacts/ArtifactDetails.tsx`).
 */
function ApprovedVersionDiff({
  approval,
  approvedVersion,
}: {
  approval: AskApproval;
  approvedVersion: number;
}): ReactNode {
  const before = useQuery({
    queryKey: ["artifact", approval.artifact_id, "version", approvedVersion],
    queryFn: () => api.getArtifactVersion(approval.artifact_id, approvedVersion),
  });
  const after = useQuery({
    queryKey: ["artifact", approval.artifact_id, "version", approval.version],
    queryFn: () => api.getArtifactVersion(approval.artifact_id, approval.version),
  });
  if (before.isError || after.isError) {
    return <p className={`text-sm ${dangerText}`}>Could not load versions to compare.</p>;
  }
  if (isText(before.data) && isText(after.data)) {
    return (
      <VersionDiff
        after={cutDecisionBlocks(after.data.markdown)}
        before={cutDecisionBlocks(before.data.markdown)}
      />
    );
  }
  return <p className={`text-sm ${textMutedOnSurface}`}>Loading versions to compare…</p>;
}

/**
 * The approval an approval request follows, when the document has one: `Approved at version N
 * on <date> by <who>.` and a **Show diff** control that reveals `ApprovedVersionDiff`. Renders
 * nothing for a first request, whose `approval` carries no earlier approval.
 */
export function EarlierApproval({ approval }: { approval: AskApproval }): ReactNode {
  const [diffOpen, setDiffOpen] = useState(false);
  const { approved_at, approved_by, approved_version } = approval;
  if (approved_version === undefined || approved_at === undefined || approved_by === undefined) {
    return null;
  }
  return (
    <div className={`mt-1 text-xs ${textMutedOnSurface}`}>
      <p>
        Approved at version {approved_version} on{" "}
        <time dateTime={approved_at} title={approved_at}>
          {approved_at.slice(0, 10)}
        </time>{" "}
        by {actorLabel(approved_by)}.
      </p>
      <DisclosureToggle
        expanded={diffOpen}
        label={diffOpen ? "Hide diff" : "Show diff"}
        onToggle={() => setDiffOpen((open) => !open)}
        textClassName={textMutedOnSurface}
      />
      {diffOpen ? (
        <ApprovedVersionDiff approval={approval} approvedVersion={approved_version} />
      ) : null}
    </div>
  );
}
