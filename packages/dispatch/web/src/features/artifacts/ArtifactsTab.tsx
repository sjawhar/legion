import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type {
  Artifact,
  ArtifactVersionContent,
  ArtifactVersionText,
  Version,
} from "../../api/types";
import { VersionDiff } from "../doc/VersionDiff";
import { buildIssuePath, parseIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { Unfurl } from "../refs/Unfurl";
import { Upload } from "./Upload";

function artifactVersionUrl(artifactID: string, version: number): string {
  return `/api/v1/artifacts/${encodeURIComponent(artifactID)}/versions/${version}`;
}

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

function formatBytes(bytes: number | undefined): string {
  if (bytes === undefined) {
    return "Size unavailable";
  }
  if (bytes < 1024) {
    return `${bytes} B`;
  }
  if (bytes < 1024 * 1024) {
    return `${(bytes / 1024).toFixed(1)} KB`;
  }
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
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
        <div className="rounded-lg border border-slate-200 bg-slate-50 p-3">
          <p className="text-xs font-medium text-slate-500">From · Version {before.number}</p>
          <p className="mt-1 text-sm text-slate-700">{formatBytes(before.size)}</p>
          <p className="mt-1 break-all text-xs text-slate-500">SHA-256 {before.sha256}</p>
        </div>
        <div className="rounded-lg border border-slate-200 bg-slate-50 p-3">
          <p className="text-xs font-medium text-slate-500">To · Version {after.number}</p>
          <p className="mt-1 text-sm text-slate-700">{formatBytes(after.size)}</p>
          <p className="mt-1 break-all text-xs text-slate-500">SHA-256 {after.sha256}</p>
        </div>
      </div>
      {identical ? <p className="text-sm text-slate-500">Identical blobs.</p> : null}
    </section>
  );
}

function isText(content: ArtifactVersionContent | undefined): content is ArtifactVersionText {
  return content !== undefined && "markdown" in content;
}

function ArtifactCard({ artifact }: { artifact: Artifact }): ReactNode {
  const queryClient = useQueryClient();
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
  const makePrimary = useMutation({
    mutationFn: () => api.makeArtifactPrimary(artifact.id),
    onSuccess: (issue) => {
      queryClient.setQueryData(["issue", issue.key], issue);
      void queryClient.invalidateQueries({ queryKey: ["artifact"] });
      void queryClient.invalidateQueries({ queryKey: ["artifacts", issue.key] });
      void queryClient.invalidateQueries({ queryKey: ["issue", issue.key] });
    },
  });
  const latestVersion = versions[0];
  const referencedBy = detail.data?.referenced_by ?? [];
  const beforeBlob = versions.find((version) => version.number === beforeVersion);
  const afterBlob = versions.find((version) => version.number === afterVersion);

  return (
    <article
      className={`space-y-3 border-b border-slate-200 py-4 last:border-b-0 dark:border-slate-800 ${
        highlighted ? "rounded-xl px-3 ring-2 ring-sky-400" : ""
      }`}
      data-testid={`artifact-${artifact.slug}`}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-center gap-2 text-slate-700 dark:text-slate-300">
          {kindIcon(artifact.kind)}
          <div className="min-w-0">
            <h2 className="truncate font-semibold text-slate-950 dark:text-slate-100">
              {artifact.name}
            </h2>
            <p className="text-xs text-slate-500 dark:text-slate-400">
              {artifact.slug} · {versions.length} {versions.length === 1 ? "version" : "versions"}
            </p>
          </div>
        </div>
        {artifact.primary ? (
          <span className="rounded-full bg-sky-100 px-2 py-1 text-xs font-semibold text-sky-800">
            Primary
          </span>
        ) : artifact.kind === "doc" ? (
          <div className="flex flex-col items-end gap-1">
            <span className="text-xs font-medium text-slate-500">Not primary</span>
            <button
              className="rounded-lg border border-slate-300 px-3 py-2 text-sm font-medium text-slate-700 hover:border-sky-500"
              disabled={makePrimary.isPending}
              onClick={() => makePrimary.mutate()}
              type="button"
            >
              Make primary
            </button>
          </div>
        ) : (
          <span title="Only documents can be primary">
            <button
              className="cursor-not-allowed rounded-lg border border-slate-200 px-3 py-2 text-sm font-medium text-slate-400"
              disabled
              title="Only documents can be primary"
              type="button"
            >
              Make primary
            </button>
          </span>
        )}
      </div>

      {artifact.kind === "image" && latestVersion !== undefined ? (
        <img
          alt={`${artifact.name} version ${latestVersion.number}`}
          className="max-h-64 w-full rounded-lg border border-slate-200 object-contain"
          loading="lazy"
          src={artifactVersionUrl(artifact.id, latestVersion.number)}
        />
      ) : null}

      <section aria-label={`Versions for ${artifact.name}`} className="space-y-2">
        <div className="flex items-center justify-between gap-3">
          <h3 className="text-sm font-semibold text-slate-700">Versions</h3>
          {versions.length > namedVersions.length ? (
            <button
              className="text-sm font-medium text-sky-700 hover:text-sky-900"
              onClick={() => setShowAllVersions((current) => !current)}
              type="button"
            >
              {showAllVersions ? "Show named versions" : "Show all versions"}
            </button>
          ) : null}
        </div>
        {displayedVersions.length === 0 ? (
          <p className="text-sm text-slate-500">No named versions yet.</p>
        ) : (
          <ul className="space-y-1">
            {displayedVersions.map((version) => (
              <li
                className="flex flex-wrap items-center justify-between gap-2 text-sm"
                key={version.number}
              >
                <span className="text-slate-700">{versionName(version)}</span>
                <a
                  className="font-medium text-sky-700 hover:text-sky-900"
                  download=""
                  href={artifactVersionUrl(artifact.id, version.number)}
                >
                  Download version {version.number}
                </a>
                <span className="w-full text-xs text-slate-500 dark:text-slate-400">
                  <Timestamp at={version.created_at} />
                  {artifact.kind === "doc"
                    ? null
                    : ` · ${formatBytes(version.size)} · SHA-256 ${version.sha256 ?? "unavailable"}`}
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>

      {versions.length >= 2 ? (
        <section aria-label={`Compare versions for ${artifact.name}`} className="space-y-2">
          <h3 className="text-sm font-semibold text-slate-700">Diff versions</h3>
          <div className="grid grid-cols-2 gap-2">
            <label className="text-xs font-medium text-slate-600">
              From
              <select
                aria-label={`Compare ${artifact.name} from`}
                className="mt-1 block w-full rounded border border-slate-300 bg-white px-2 py-2 text-sm"
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
            <label className="text-xs font-medium text-slate-600">
              To
              <select
                aria-label={`Compare ${artifact.name} to`}
                className="mt-1 block w-full rounded border border-slate-300 bg-white px-2 py-2 text-sm"
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
              <p className="text-sm text-rose-700">Could not load versions to compare.</p>
            ) : isText(beforeContent.data) && isText(afterContent.data) ? (
              <VersionDiff
                after={afterContent.data.markdown}
                before={beforeContent.data.markdown}
              />
            ) : (
              <p className="text-sm text-slate-500">Loading versions to compare…</p>
            )
          ) : beforeBlob !== undefined && afterBlob !== undefined ? (
            <BlobVersionComparison after={afterBlob} before={beforeBlob} />
          ) : (
            <p className="text-sm text-slate-500">Select two versions to compare.</p>
          )}
        </section>
      ) : null}

      {detail.isError ? <p className="text-sm text-rose-700">Could not load references.</p> : null}
      {referencedBy.length === 0 ? null : (
        <section aria-label="Referenced by" className="space-y-2">
          <h3 className="text-sm font-semibold text-slate-700">Referenced by</h3>
          {referencedBy.map((reference) => (
            <article
              className="rounded-lg border border-slate-200 p-3"
              key={`${reference.kind}:${reference.id}`}
            >
              <p className="text-xs font-medium text-slate-500">
                {reference.kind[0]?.toUpperCase()}
                {reference.kind.slice(1)} ·{" "}
                <Link
                  className="text-sky-700 underline"
                  to={buildIssuePath({ key: reference.issue_key, kind: "issue" })}
                >
                  {reference.issue_key}
                </Link>
              </p>
              <Unfurl body={reference.excerpt} />
            </article>
          ))}
        </section>
      )}
      {makePrimary.isError ? (
        <p className="text-sm text-rose-700">Could not select this document.</p>
      ) : null}
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
    return <p className="pt-3 text-sm text-slate-500">Open an issue to view its artifacts.</p>;
  }
  if (artifacts.isPending) {
    return <p className="pt-3 text-sm text-slate-500">Loading artifacts…</p>;
  }
  if (artifacts.isError) {
    return <p className="pt-3 text-sm text-rose-700">Could not load artifacts.</p>;
  }

  return (
    <div className="space-y-1 pt-3">
      <Upload issueKey={issueKey} />
      {orderedArtifacts.map((artifact) => (
        <ArtifactCard artifact={artifact} key={artifact.id} />
      ))}
    </div>
  );
}
