import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type {
  Artifact,
  ArtifactVersionContent,
  ArtifactVersionText,
  Version,
} from "../../api/types";
import {
  badgePrimary,
  borderDefault,
  dangerText,
  highlightRing,
  inputClasses,
  linkHoverText,
  linkText,
  surfaceMutedBg,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { VersionDiff } from "../doc/VersionDiff";
import { buildIssuePath, parseIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { Unfurl } from "../refs/Unfurl";
import { artifactVersionUrl, formatArtifactBytes } from "./ArtifactHeader";
import { Upload } from "./Upload";

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

function versionName(version: Version): string {
  return `Version ${version.number}${version.summary === null ? "" : ` — ${version.summary}`}`;
}

export function BlobVersionComparison({
  after,
  before,
}: {
  after: Version;
  before: Version;
}): ReactNode {
  const identical = before.size === after.size && before.sha256 === after.sha256;

  return (
    <section aria-label="Blob version comparison" className="space-y-2">
      <div className="grid grid-cols-2 gap-2">
        <div className={`rounded-lg p-3 ${borderDefault} ${surfaceMutedBg}`}>
          <p className={`text-xs font-medium ${textMutedOnSurfaceMuted}`}>
            From · Version {before.number}
          </p>
          <p className={`mt-1 text-sm ${textSecondaryOnSurface}`}>
            {formatArtifactBytes(before.size)}
          </p>
          <p className={`mt-1 break-all text-xs ${textMutedOnSurfaceMuted}`}>
            SHA-256 {before.sha256}
          </p>
        </div>
        <div className={`rounded-lg p-3 ${borderDefault} ${surfaceMutedBg}`}>
          <p className={`text-xs font-medium ${textMutedOnSurfaceMuted}`}>
            To · Version {after.number}
          </p>
          <p className={`mt-1 text-sm ${textSecondaryOnSurface}`}>
            {formatArtifactBytes(after.size)}
          </p>
          <p className={`mt-1 break-all text-xs ${textMutedOnSurfaceMuted}`}>
            SHA-256 {after.sha256}
          </p>
        </div>
      </div>
      {identical ? <p className={`text-sm ${textMutedOnSurface}`}>Identical blobs.</p> : null}
    </section>
  );
}

function isText(content: ArtifactVersionContent | undefined): content is ArtifactVersionText {
  return content !== undefined && "markdown" in content;
}

function ArtifactCard({ artifact, issueKey }: { artifact: Artifact; issueKey: string }): ReactNode {
  const { pathname } = useLocation();
  const route = parseIssuePath(pathname);
  const highlighted = route?.kind === "artifact" && route.slug === artifact.slug;
  const [showAllVersions, setShowAllVersions] = useState(false);
  const versions = [...artifact.versions].sort((left, right) => right.number - left.number);
  const namedVersions = versions.filter((version) => version.named);
  const displayedVersions = showAllVersions ? versions : namedVersions;
  const beforeDefault = versions[1]?.number;
  const afterDefault = versions[0]?.number;
  const [before, setBefore] = useState<number | undefined>();
  const [after, setAfter] = useState<number | undefined>();
  const beforeVersion = before ?? beforeDefault;
  const afterVersion = after ?? afterDefault;
  const detail = useQuery({
    queryKey: ["artifact", artifact.id],
    queryFn: () => api.getArtifact(artifact.id),
  });
  const beforeContent = useQuery({
    enabled: artifact.kind === "doc" && beforeVersion !== undefined,
    queryKey: ["artifact", artifact.id, "version", beforeVersion],
    queryFn: () => api.getArtifactVersion(artifact.id, beforeVersion ?? 0),
  });
  const afterContent = useQuery({
    enabled: artifact.kind === "doc" && afterVersion !== undefined,
    queryKey: ["artifact", artifact.id, "version", afterVersion],
    queryFn: () => api.getArtifactVersion(artifact.id, afterVersion ?? 0),
  });
  const latestVersion = versions[0];
  const referencedBy = detail.data?.referenced_by ?? [];
  const beforeBlob = versions.find((version) => version.number === beforeVersion);
  const afterBlob = versions.find((version) => version.number === afterVersion);

  return (
    <article
      className={`space-y-3 py-4 last:border-b-0 ${borderDefault} ${
        highlighted ? `rounded-xl px-3 ${highlightRing}` : ""
      }`}
      data-testid={`artifact-${artifact.slug}`}
    >
      <div className="flex items-start justify-between gap-3">
        <div className={`flex min-w-0 items-center gap-2 ${textSecondaryOnSurface}`}>
          {kindIcon(artifact.kind)}
          <div className="min-w-0">
            <h2 className={`truncate font-semibold ${textPrimaryOnSurface}`}>
              <Link
                className={`underline ${linkText} ${linkHoverText}`}
                to={buildIssuePath({
                  key: issueKey,
                  kind: "artifact",
                  slug: artifact.slug,
                })}
              >
                {artifact.name}
              </Link>
            </h2>
            <p className={`text-xs ${textMutedOnSurface}`}>
              {artifact.kind} · {artifact.slug} · {versions.length}{" "}
              {versions.length === 1 ? "version" : "versions"} · Created{" "}
              <Timestamp at={artifact.created_at} />
            </p>
          </div>
        </div>
        {artifact.primary ? (
          <span
            className={`rounded-full px-2 py-1 text-xs font-semibold ${badgePrimary.bg} ${badgePrimary.text}`}
          >
            Primary
          </span>
        ) : null}
      </div>

      {artifact.kind === "image" && latestVersion !== undefined ? (
        <img
          alt={`${artifact.name} version ${latestVersion.number}`}
          className={`max-h-64 w-full rounded-lg border object-contain ${borderDefault}`}
          loading="lazy"
          src={artifactVersionUrl(artifact.id, latestVersion.number)}
        />
      ) : null}

      <section aria-label={`Versions for ${artifact.name}`} className="space-y-2">
        <div className="flex items-center justify-between gap-3">
          <h3 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Versions</h3>
          {versions.length > namedVersions.length ? (
            <button
              className={`min-h-11 text-sm font-medium ${linkText} ${linkHoverText}`}
              onClick={() => setShowAllVersions((current) => !current)}
              type="button"
            >
              {showAllVersions ? "Show named versions" : "Show all versions"}
            </button>
          ) : null}
        </div>
        {displayedVersions.length === 0 ? (
          <p className={`text-sm ${textMutedOnSurface}`}>No named versions yet.</p>
        ) : (
          <ul className="space-y-1">
            {displayedVersions.map((version) => (
              <li
                className="flex flex-wrap items-center justify-between gap-2 text-sm"
                key={version.number}
              >
                <span className={textSecondaryOnSurface}>{versionName(version)}</span>
                <a
                  className={`inline-flex min-h-11 items-center font-medium ${linkText} ${linkHoverText}`}
                  download=""
                  href={artifactVersionUrl(artifact.id, version.number)}
                >
                  Download version {version.number}
                </a>
                <span className={`w-full text-xs ${textMutedOnSurface}`}>
                  <Timestamp at={version.created_at} />
                  {artifact.kind === "doc"
                    ? null
                    : ` · ${formatArtifactBytes(version.size)} · SHA-256 ${version.sha256 ?? "unavailable"}`}
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>

      {versions.length >= 2 ? (
        <section aria-label={`Compare versions for ${artifact.name}`} className="space-y-2">
          <h3 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Diff versions</h3>
          <div className="grid grid-cols-2 gap-2">
            <label className={`text-xs font-medium ${textSecondaryOnSurface}`}>
              From
              <select
                aria-label={`Compare ${artifact.name} from`}
                className={`mt-1 block min-h-11 w-full rounded border px-2 py-2 text-sm ${inputClasses(true)}`}
                onChange={(event) => setBefore(Number(event.target.value))}
                value={beforeVersion}
              >
                {versions.map((version) => (
                  <option key={version.number} value={version.number}>
                    {versionName(version)}
                  </option>
                ))}
              </select>
            </label>
            <label className={`text-xs font-medium ${textSecondaryOnSurface}`}>
              To
              <select
                aria-label={`Compare ${artifact.name} to`}
                className={`mt-1 block min-h-11 w-full rounded border px-2 py-2 text-sm ${inputClasses(true)}`}
                onChange={(event) => setAfter(Number(event.target.value))}
                value={afterVersion}
              >
                {versions.map((version) => (
                  <option key={version.number} value={version.number}>
                    {versionName(version)}
                  </option>
                ))}
              </select>
            </label>
          </div>
          {artifact.kind === "doc" ? (
            beforeContent.isError || afterContent.isError ? (
              <p className={`text-sm ${dangerText}`}>Could not load versions to compare.</p>
            ) : isText(beforeContent.data) && isText(afterContent.data) ? (
              <VersionDiff
                after={afterContent.data.markdown}
                before={beforeContent.data.markdown}
              />
            ) : (
              <p className={`text-sm ${textMutedOnSurface}`}>Loading versions to compare…</p>
            )
          ) : beforeBlob !== undefined && afterBlob !== undefined ? (
            <BlobVersionComparison after={afterBlob} before={beforeBlob} />
          ) : (
            <p className={`text-sm ${textMutedOnSurface}`}>Select two versions to compare.</p>
          )}
        </section>
      ) : null}

      {detail.isError ? (
        <p className={`text-sm ${dangerText}`}>Could not load references.</p>
      ) : null}
      {referencedBy.length === 0 ? null : (
        <section aria-label="Referenced by" className="space-y-2">
          <h3 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Referenced by</h3>
          {referencedBy.map((reference) => {
            if (reference.issue_key === null) return null;
            return (
              <article
                className={`rounded-lg border p-3 ${borderDefault}`}
                key={`${reference.kind}:${reference.id}`}
              >
                <p className={`text-xs font-medium ${textMutedOnSurface}`}>
                  {reference.kind[0]?.toUpperCase()}
                  {reference.kind.slice(1)} ·{" "}
                  <Link
                    className={`underline ${linkText}`}
                    to={buildIssuePath({ key: reference.issue_key, kind: "issue" })}
                  >
                    {reference.issue_key}
                  </Link>
                </p>
                <Unfurl body={reference.excerpt} />
              </article>
            );
          })}
        </section>
      )}
    </article>
  );
}

export function ArtifactsTab(): ReactNode {
  const { pathname } = useLocation();
  const issueKey = parseIssuePath(pathname)?.key;
  const artifacts = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["artifacts", issueKey],
    queryFn: () => api.listArtifacts(issueKey ?? ""),
  });
  const orderedArtifacts = [...(artifacts.data ?? [])].sort((left, right) => {
    if (left.primary !== right.primary) {
      return left.primary ? -1 : 1;
    }
    const leftUpdated = left.versions.at(-1)?.created_at ?? left.created_at;
    const rightUpdated = right.versions.at(-1)?.created_at ?? right.created_at;
    return rightUpdated.localeCompare(leftUpdated);
  });

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
    <div className="space-y-1 pt-3">
      <Upload owner={{ issue: issueKey }} />
      {orderedArtifacts.map((artifact) => (
        <ArtifactCard artifact={artifact} issueKey={issueKey} key={artifact.id} />
      ))}
    </div>
  );
}
