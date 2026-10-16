import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import { badgeLow, badgePrimary } from "../../theme/classes";
import { badgeSelectBadge, badgeSelectOverlay, badgeSelectRailWrapper } from "./badge-select";
import { useIssueRepo } from "./useIssueRepo";

/**
 * The issue's repository editor: a badge naming the repository the issue resolves to (its own,
 * else its nearest ancestor's, else the project's default, muted when not its own; "No repository"
 * when none resolves) with a native `<select>` laid over it, as `AssigneeControl` does. The select
 * lists Inherit first, then the repositories mapped to the issue's project in Settings, read only
 * once the reader reaches for the control (pointer over it, a touch on it, or focus). Inherit
 * clears the issue's own repository. Labelled `Repository of <KEY>`.
 */
export function RepoControl({
  disabled = false,
  issueKey,
  project,
  repo,
  resolvedRepo,
}: {
  disabled?: boolean;
  issueKey: string;
  project: string;
  repo: string | null;
  resolvedRepo: string | null;
}): ReactNode {
  const [wanted, setWanted] = useState(false);
  const mappings = useQuery({
    enabled: wanted,
    queryKey: ["repo-projects"],
    queryFn: () => api.listRepoProjects(),
  });
  const want = () => setWanted(true);
  const write = useIssueRepo(issueKey);
  const mapped = (mappings.data ?? [])
    .filter((mapping) => mapping.project === project)
    .map((mapping) => mapping.repo)
    .sort();
  // The issue's own repository stays selectable before the list loads, so the control never
  // shows a value its options lack.
  const options = repo === null || mapped.includes(repo) ? mapped : [repo, ...mapped];
  const tone = repo === null ? badgeLow : badgePrimary;
  return (
    <>
      <span
        className={`${badgeSelectRailWrapper} ${disabled ? "opacity-50" : ""}`}
        title={
          repo === null && resolvedRepo !== null ? "Inherited or the project's default" : undefined
        }
      >
        <span
          aria-hidden="true"
          className={`${badgeSelectBadge} font-mono ${tone.bg} ${tone.text}`}
        >
          {resolvedRepo ?? "No repository"}
        </span>
        <select
          aria-label={`Repository of ${issueKey}`}
          className={badgeSelectOverlay}
          disabled={disabled}
          onFocus={want}
          onMouseDown={(event) => event.stopPropagation()}
          onPointerDown={(event) => event.stopPropagation()}
          onPointerEnter={want}
          onTouchStart={(event) => {
            event.stopPropagation();
            want();
          }}
          onChange={(event) => {
            const { value } = event.target;
            write.submit(value === "" ? null : value);
          }}
          value={repo ?? ""}
        >
          <option value="">Inherit</option>
          {options.map((candidate) => (
            <option key={candidate} value={candidate}>
              {candidate}
            </option>
          ))}
        </select>
      </span>
      {write.failed ? (
        <QueryError
          message={write.error ?? `Could not update the repository of ${issueKey}.`}
          onRetry={write.retry}
          retrying={write.pending}
        />
      ) : null}
    </>
  );
}
