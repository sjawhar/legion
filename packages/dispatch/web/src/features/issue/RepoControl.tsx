import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import { badgeLow, badgePrimary, textSecondaryOnSurface } from "../../theme/classes";
import { badgeSelectBadge, badgeSelectOverlay, badgeSelectRailWrapper } from "./badge-select";
import { useIssueRepo } from "./useIssueRepo";

/**
 * The issue's repository in the details rail: `Repository:` and a badge naming the repository the
 * issue resolves to (its own, else its nearest ancestor's, else the project's default, muted when
 * not its own; "No repository" when none resolves) with a native `<select>` laid over it, as
 * `AssigneeControl` does. The select lists Inherit first, then the repositories mapped to the
 * issue's project in Settings; Inherit clears the issue's own repository. Labelled
 * `Repository of <KEY>`. While the issue resolves to a repository the mappings are read only once
 * the reader reaches for the control (pointer over it, a touch on it, or focus). While it resolves
 * to none, they are read at once, because the item shows only when the project maps a repository:
 * until the read answers nothing shows, a project that maps none has nothing to pick and gets no
 * item, and a read that fails shows `Repository:` with the failure and a Retry, never a hidden
 * item. A failed read beside a resolved repository shows the same failure next to the picker. The
 * badge is hidden from assistive technology; the select's description names what it shows: the
 * repository, `, inherited or the project's default` when it is not the issue's own, or
 * `No repository`.
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
    enabled: wanted || resolvedRepo === null,
    queryKey: ["repo-projects"],
    queryFn: () => api.listRepoProjects(),
  });
  const want = () => setWanted(true);
  const descriptionId = useId();
  const write = useIssueRepo(issueKey);
  const mapped = (mappings.data ?? [])
    .filter((mapping) => mapping.project === project)
    .map((mapping) => mapping.repo)
    .sort();
  const loadFailure = mappings.isError ? (
    <QueryError
      message={apiErrorMessage(
        mappings.error,
        `Could not load the repositories mapped to ${project}.`
      )}
      onRetry={() => void mappings.refetch()}
      retrying={mappings.isFetching}
    />
  ) : null;
  if (resolvedRepo === null && !mappings.isError && (mappings.isPending || mapped.length === 0)) {
    return null;
  }
  // The issue's own repository stays selectable before the list loads, so the control never
  // shows a value its options lack.
  const options = repo === null || mapped.includes(repo) ? mapped : [repo, ...mapped];
  const tone = repo === null ? badgeLow : badgePrimary;
  const description =
    resolvedRepo === null
      ? "No repository"
      : repo === null
        ? `${resolvedRepo}, inherited or the project's default`
        : resolvedRepo;
  // Nothing resolved and no list to pick from: the failure is all there is to show.
  const picker =
    resolvedRepo === null && mappings.isError ? null : (
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
          aria-describedby={descriptionId}
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
        <span className="sr-only" id={descriptionId}>
          {description}
        </span>
      </span>
    );
  return (
    <div className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
      <span className="font-medium">Repository:</span>
      {picker}
      {loadFailure}
      {write.failed ? (
        <QueryError
          message={write.error ?? `Could not update the repository of ${issueKey}.`}
          onRetry={write.retry}
          retrying={write.pending}
        />
      ) : null}
    </div>
  );
}
