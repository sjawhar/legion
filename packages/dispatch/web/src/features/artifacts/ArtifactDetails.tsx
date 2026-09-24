import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import type {
  Artifact,
  ArtifactVersionContent,
  ArtifactVersionText,
  Version,
} from "../../api/types";
import {
  borderDefault,
  dangerText,
  inputClasses,
  linkText,
  surfaceMutedBg,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { VersionDiff } from "../doc/VersionDiff";
import { ReferencedBy } from "../refs/ReferencedBy";
import { buildDispatchReference, documentRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import {
  artifactVersionUrl,
  formatArtifactBytes,
  versionLabel,
  versionsNewestFirst,
} from "./ArtifactHeader";

function isText(content: ArtifactVersionContent | undefined): content is ArtifactVersionText {
  return content !== undefined && "markdown" in content;
}

// A blob (image/file) has no markdown to diff, so its "compare" is size and checksum, not a
// text diff — this is what a document's From/To compare falls back to when either side lacks
// text content.
function BlobVersionComparison({ after, before }: { after: Version; before: Version }): ReactNode {
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

/** One side of the From/To compare: a labelled picker over every version, newest first. */
function CompareVersionSelect({
  ariaLabel,
  label,
  onChange,
  value,
  versions,
}: {
  ariaLabel: string;
  label: string;
  onChange: (version: number) => void;
  value: number | undefined;
  versions: readonly Version[];
}): ReactNode {
  return (
    <label className={`text-xs font-medium ${textSecondaryOnSurface}`}>
      {label}
      <select
        aria-label={ariaLabel}
        className={`mt-1 block min-h-11 w-full rounded border px-2 py-2 text-sm ${inputClasses(true)}`}
        onChange={(event) => onChange(Number(event.target.value))}
        value={value}
      >
        {versions.map((version) => (
          <option key={version.number} value={version.number}>
            {versionLabel(version)}
          </option>
        ))}
      </select>
    </label>
  );
}

// Rendered below the document/blob on the artifact page only: the version history, a From/To
// compare across any two versions, and the artifact's own inbound references. The page's
// `ArtifactHeader` already offers a version picker plus "Diff vs current" for documents (only
// current vs. one historical version); this From/To compare covers the arbitrary-pair case
// (e.g. version 3 vs version 7) that picker can't, for both docs and blobs.
export function ArtifactDetails({ artifact }: { artifact: Artifact }): ReactNode {
  const [showAllVersions, setShowAllVersions] = useState(false);
  // A diff is render-heavy and long; it appears only after the reader asks for it.
  const [compareOpen, setCompareOpen] = useState(false);
  const versions = versionsNewestFirst(artifact.versions);
  const namedVersions = versions.filter((version) => version.named);
  const displayedVersions = showAllVersions ? versions : namedVersions;
  const beforeDefault = versions[1]?.number;
  const afterDefault = versions[0]?.number;
  const [before, setBefore] = useState<number | undefined>();
  const [after, setAfter] = useState<number | undefined>();
  const beforeVersion = before ?? beforeDefault;
  const afterVersion = after ?? afterDefault;
  const beforeContent = useQuery({
    enabled: compareOpen && artifact.kind === "doc" && beforeVersion !== undefined,
    queryKey: ["artifact", artifact.id, "version", beforeVersion],
    queryFn: () => api.getArtifactVersion(artifact.id, beforeVersion ?? 0),
  });
  const afterContent = useQuery({
    enabled: compareOpen && artifact.kind === "doc" && afterVersion !== undefined,
    queryKey: ["artifact", artifact.id, "version", afterVersion],
    queryFn: () => api.getArtifactVersion(artifact.id, afterVersion ?? 0),
  });
  const beforeBlob = versions.find((version) => version.number === beforeVersion);
  const afterBlob = versions.find((version) => version.number === afterVersion);

  return (
    <div className="space-y-4">
      <section aria-label={`Versions for ${artifact.name}`} className="space-y-2">
        <div className="flex items-center justify-between gap-3">
          <h3 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Versions</h3>
          {versions.length > namedVersions.length ? (
            <button
              className={`min-h-11 text-sm font-medium ${linkText}`}
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
                <span className={textSecondaryOnSurface}>{versionLabel(version)}</span>
                <a
                  className={`inline-flex min-h-11 items-center font-medium ${linkText}`}
                  download=""
                  href={artifactVersionUrl(artifact.id, version.number)}
                >
                  Download version {version.number}
                </a>
                <span className={`w-full break-all text-xs ${textMutedOnSurface}`}>
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
          <div className="flex items-center justify-between gap-3">
            <h3 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Compare versions</h3>
            <button
              aria-expanded={compareOpen}
              className={`min-h-11 text-sm font-medium ${linkText}`}
              onClick={() => setCompareOpen((current) => !current)}
              type="button"
            >
              {compareOpen ? "Hide comparison" : "Show comparison"}
            </button>
          </div>
          {compareOpen ? (
            <>
              <div className="grid grid-cols-2 gap-2">
                <CompareVersionSelect
                  ariaLabel={`Compare ${artifact.name} from`}
                  label="From"
                  onChange={setBefore}
                  value={beforeVersion}
                  versions={versions}
                />
                <CompareVersionSelect
                  ariaLabel={`Compare ${artifact.name} to`}
                  label="To"
                  onChange={setAfter}
                  value={afterVersion}
                  versions={versions}
                />
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
            </>
          ) : null}
        </section>
      ) : null}

      <ReferencedBy reference={buildDispatchReference(documentRoute(artifact))} />
    </div>
  );
}
