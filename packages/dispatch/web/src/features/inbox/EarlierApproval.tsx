import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";

import { isText } from "../../api/artifact-version";
import { api } from "../../api/client";
import type { AskApproval } from "../../api/types";
import { DisclosureToggle } from "../../components/DisclosureToggle";
import { dangerText, textMutedOnSurface } from "../../theme/classes";
import { cutDecisionBlocks } from "../doc/decision-blocks";
import { VersionDiff } from "../doc/VersionDiff";
import { actorLabel } from "../refs/actor";

/**
 * The diff between the version a human approved and the version the request names, with every
 * decision block cut out of both (`cutDecisionBlocks`), so it shows what changed outside them:
 * the answers to decision blocks between the two are what the human himself decided. Mounted
 * only once the reader asks for it, so nothing is fetched or rendered before. Both reads share
 * the document page's `["artifact", id, "version", n]` keys, and the loading and error copy is
 * its comparison's (`features/artifacts/ArtifactDetails.tsx`).
 */
function ApprovedVersionDiff({
  approvedVersion,
  artifactId,
  version,
}: {
  approvedVersion: number;
  artifactId: string;
  version: number;
}): ReactNode {
  const before = useQuery({
    queryKey: ["artifact", artifactId, "version", approvedVersion],
    queryFn: () => api.getArtifactVersion(artifactId, approvedVersion),
  });
  const after = useQuery({
    queryKey: ["artifact", artifactId, "version", version],
    queryFn: () => api.getArtifactVersion(artifactId, version),
  });
  // The cut runs once per fetched pair, not once per render: the card re-renders on every
  // keystroke in its reason field while the diff stays open.
  const beforeMarkdown = isText(before.data) ? before.data.markdown : undefined;
  const afterMarkdown = isText(after.data) ? after.data.markdown : undefined;
  const cut = useMemo(
    () =>
      beforeMarkdown === undefined || afterMarkdown === undefined
        ? undefined
        : { after: cutDecisionBlocks(afterMarkdown), before: cutDecisionBlocks(beforeMarkdown) },
    [afterMarkdown, beforeMarkdown]
  );
  if (before.isError || after.isError) {
    return <p className={`text-sm ${dangerText}`}>Could not load versions to compare.</p>;
  }
  if (cut !== undefined) {
    return <VersionDiff after={cut.after} before={cut.before} />;
  }
  return <p className={`text-sm ${textMutedOnSurface}`}>Loading versions to compare…</p>;
}

/**
 * The approval an approval request follows, when the document has one: `Approved at version N
 * on <date> by <who>.` and a **Show diff** control that reveals `ApprovedVersionDiff`. Renders
 * nothing for a first request, whose `approval` carries no `approved`.
 */
export function EarlierApproval({ approval }: { approval: AskApproval }): ReactNode {
  const [diffOpen, setDiffOpen] = useState(false);
  const { approved } = approval;
  if (approved === undefined) {
    return null;
  }
  return (
    <div className={`mt-1 text-xs ${textMutedOnSurface}`}>
      <p>
        Approved at version {approved.version} on{" "}
        <time dateTime={approved.at} title={approved.at}>
          {approved.at.slice(0, 10)}
        </time>{" "}
        by {actorLabel(approved.by)}.
      </p>
      <DisclosureToggle
        expanded={diffOpen}
        label={diffOpen ? "Hide diff" : "Show diff"}
        onToggle={() => setDiffOpen((open) => !open)}
        textClassName={textMutedOnSurface}
      />
      {diffOpen ? (
        <ApprovedVersionDiff
          approvedVersion={approved.version}
          artifactId={approval.artifact_id}
          version={approval.version}
        />
      ) : null}
    </div>
  );
}
