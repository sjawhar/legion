import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";
import { Link, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, IssueReferences } from "../../api/types";
import {
  badgePrimary,
  borderDefault,
  dangerText,
  highlightRing,
  inputClasses,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { buildIssuePath, buildProjectPath, parseIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { artifactVersionUrl } from "./ArtifactHeader";
import { ArtifactDropZone, ArtifactUploadRow, useArtifactUpload } from "./ArtifactUpload";

// Re-exported so existing importers of the per-artifact "referenced by" list (the project
// document page) keep resolving; the list itself now lives with the rest of an artifact's
// details in ArtifactDetails.tsx, rendered on the artifact page rather than the tab.
export { ReferencedBy } from "./ArtifactDetails";

function kindIcon(kind: Artifact["kind"]): ReactNode {
  const common = {
    "aria-hidden": true,
    className: "size-5 shrink-0",
    fill: "none",
    viewBox: "0 0 24 24",
  };
  if (kind === "doc") {
    return (
      <svg {...common} stroke="currentColor">
        <title>Document artifact</title>
        <path
          d="M7 3h7l3 3v15H7z"
          strokeLinecap="round"
          strokeLinejoin="round"
          strokeWidth="1.75"
        />
        <path d="M14 3v4h4M10 12h4M10 16h4" strokeLinecap="round" strokeWidth="1.75" />
      </svg>
    );
  }
  if (kind === "image") {
    return (
      <svg {...common} stroke="currentColor">
        <title>Image artifact</title>
        <rect height="16" rx="2" width="18" x="3" y="4" strokeWidth="1.75" />
        <circle cx="9" cy="10" r="1.25" strokeWidth="1.75" />
        <path
          d="m5 18 5-5 3 3 2-2 4 4"
          strokeLinecap="round"
          strokeLinejoin="round"
          strokeWidth="1.75"
        />
      </svg>
    );
  }
  return (
    <svg {...common} stroke="currentColor">
      <title>File artifact</title>
      <path d="M7 3h7l3 3v15H7z" strokeLinecap="round" strokeLinejoin="round" strokeWidth="1.75" />
      <path d="M14 3v4h4" strokeLinecap="round" strokeLinejoin="round" strokeWidth="1.75" />
    </svg>
  );
}

function artifactPath(artifact: Artifact): string {
  return artifact.issue_key === null
    ? buildProjectPath({
        kind: "document",
        project: artifact.project,
        slug: artifact.slug,
      })
    : buildIssuePath({
        key: artifact.issue_key,
        kind: "artifact",
        slug: artifact.slug,
      });
}

// The issue's reference closure (every artifact reachable by following references, up to 8
// hops) — a different thing from a single artifact's own "Referenced by" list above. Collapsed
// by default: it is background context for the issue, not something worth scrolling past on
// every visit to the tab.
function References({ references }: { references: IssueReferences }): ReactNode {
  if (references.members.length === 0) {
    return null;
  }

  return (
    <details className={`rounded-lg border p-3 ${borderDefault}`}>
      <summary
        className={`min-h-11 cursor-pointer text-sm font-semibold ${textSecondaryOnSurface}`}
      >
        References ({references.members.length})
      </summary>
      <section aria-label="References" className="mt-3 space-y-2">
        <ul className="space-y-2">
          {references.members.map(({ artifact, depth, via }) => (
            <li className={`rounded-lg border p-3 text-sm ${borderDefault}`} key={artifact.id}>
              <Link
                className={`font-medium underline ${linkText} ${linkHoverText}`}
                to={artifactPath(artifact)}
              >
                {artifact.name}
              </Link>
              <p className={textMutedOnSurface}>
                {artifact.issue_key ?? artifact.project} · via {via.kind} · depth {depth}
              </p>
            </li>
          ))}
        </ul>
        {references.truncated ? (
          <p className={`text-sm ${textMutedOnSurface}`}>more references beyond 8 hops</p>
        ) : null}
      </section>
    </details>
  );
}

function ArtifactRow({
  artifact,
  highlighted,
  issueKey,
}: {
  artifact: Artifact;
  highlighted: boolean;
  issueKey: string;
}): ReactNode {
  const versions = [...artifact.versions].sort((left, right) => right.number - left.number);
  const latestVersion = versions[0];

  return (
    <li
      className={`flex items-center gap-3 border-b py-3 last:border-b-0 ${borderDefault} ${
        highlighted ? `rounded-xl px-3 ${highlightRing}` : ""
      }`}
      data-testid={`artifact-${artifact.slug}`}
    >
      <span className={`shrink-0 ${textSecondaryOnSurface}`}>{kindIcon(artifact.kind)}</span>
      {artifact.kind === "image" && latestVersion !== undefined ? (
        <img
          alt={`${artifact.name} version ${latestVersion.number}`}
          className={`h-10 w-10 shrink-0 rounded border object-cover ${borderDefault}`}
          loading="lazy"
          src={artifactVersionUrl(artifact.id, latestVersion.number)}
        />
      ) : null}
      <div className="min-w-0 flex-1">
        <Link
          className={`block truncate font-medium underline ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: issueKey, kind: "artifact", slug: artifact.slug })}
        >
          {artifact.name}
        </Link>
        <p className={`truncate text-xs ${textMutedOnSurface}`}>
          {artifact.kind} · {versions.length} {versions.length === 1 ? "version" : "versions"} ·
          Updated <Timestamp at={latestVersion?.created_at ?? artifact.created_at} />
        </p>
      </div>
      {artifact.primary ? (
        <span
          className={`shrink-0 rounded-full px-2 py-1 text-xs font-semibold ${badgePrimary.bg} ${badgePrimary.text}`}
        >
          Primary
        </span>
      ) : null}
      <ApprovalChip artifact={artifact} />
      {latestVersion === undefined ? null : (
        <a
          className={`shrink-0 text-sm font-medium underline ${linkText} ${linkHoverText}`}
          download=""
          href={artifactVersionUrl(artifact.id, latestVersion.number)}
        >
          Download version {latestVersion.number}
        </a>
      )}
    </li>
  );
}

export function ArtifactsTab(): ReactNode {
  const { pathname } = useLocation();
  const route = parseIssuePath(pathname);
  const issueKey = route?.key;
  const highlightedSlug = route?.kind === "artifact" ? route.slug : undefined;
  const [filter, setFilter] = useState("");
  const upload = useArtifactUpload({ issue: issueKey ?? "" });
  const artifacts = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["artifacts", issueKey],
    queryFn: () => {
      if (issueKey === undefined) {
        throw new Error("Artifact list requires an issue route.");
      }
      return api.listArtifacts(issueKey);
    },
  });
  const references = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["issue", issueKey, "references"],
    queryFn: () => {
      if (issueKey === undefined) {
        throw new Error("Issue references require an issue route.");
      }
      return api.getIssueReferences(issueKey);
    },
  });
  const orderedArtifacts = useMemo(
    () =>
      [...(artifacts.data ?? [])].sort((left, right) => {
        if (left.primary !== right.primary) {
          return left.primary ? -1 : 1;
        }
        const leftUpdated = left.versions.at(-1)?.created_at ?? left.created_at;
        const rightUpdated = right.versions.at(-1)?.created_at ?? right.created_at;
        return rightUpdated.localeCompare(leftUpdated);
      }),
    [artifacts.data]
  );
  const filterQuery = filter.trim().toLocaleLowerCase();
  const visibleArtifacts =
    filterQuery === ""
      ? orderedArtifacts
      : orderedArtifacts.filter(
          (artifact) =>
            artifact.name.toLocaleLowerCase().includes(filterQuery) ||
            artifact.kind.toLocaleLowerCase().includes(filterQuery)
        );

  if (issueKey === undefined) {
    return (
      <p className={`pt-3 text-sm ${textMutedOnSurface}`}>Open an issue to view its artifacts.</p>
    );
  }
  if (artifacts.isPending) {
    return <p className={`pt-3 text-sm ${textMutedOnSurface}`}>Loading artifacts…</p>;
  }
  if (artifacts.isError) {
    return <p className={`pt-3 text-sm ${dangerText}`}>Could not load artifacts.</p>;
  }

  return (
    <div className="space-y-3 pt-3">
      <ArtifactUploadRow upload={upload} />
      {references.data === undefined ? null : <References references={references.data} />}
      <label className="block">
        <span className="sr-only">Filter artifacts</span>
        <input
          aria-label="Filter artifacts"
          className={`block min-h-11 w-full rounded-lg border px-3 py-2 text-sm ${inputClasses(true)}`}
          onChange={(event) => setFilter(event.target.value)}
          placeholder="Filter artifacts"
          type="search"
          value={filter}
        />
      </label>
      <ArtifactDropZone dropTarget={upload.dropTarget}>
        {visibleArtifacts.length === 0 ? (
          <p className={`text-sm ${textMutedOnSurface}`}>
            {orderedArtifacts.length === 0
              ? "No artifacts yet."
              : "No artifacts match this filter."}
          </p>
        ) : (
          <ul className={`rounded-lg border px-3 ${borderDefault}`}>
            {visibleArtifacts.map((artifact) => (
              <ArtifactRow
                artifact={artifact}
                highlighted={artifact.slug === highlightedSlug}
                issueKey={issueKey}
                key={artifact.id}
              />
            ))}
          </ul>
        )}
      </ArtifactDropZone>
    </div>
  );
}
