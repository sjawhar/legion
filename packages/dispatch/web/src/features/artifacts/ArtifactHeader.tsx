import type { ReactNode } from "react";
import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import type { Artifact, Version } from "../../api/types";
import {
  borderDefault,
  card,
  dangerText,
  highlightRing,
  inputClasses,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { ConnectionDot } from "../doc/ConnectionDot";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";

export function artifactVersionUrl(artifactID: string, version: number): string {
  return `/api/v1/artifacts/${encodeURIComponent(artifactID)}/versions/${version}`;
}

export function formatArtifactBytes(bytes: number | undefined): string {
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

export function versionLabel(version: Version): string {
  return `Version ${version.number}${version.summary === null ? "" : ` — ${version.summary}`}`;
}

export function ArtifactHeader({
  artifact,
  children,
  highlight,
  isClosed = false,
  onShowDiffChange,
  showDiff = false,
  showVersionPicker,
  toolbar,
  version,
}: {
  artifact: Artifact;
  children: ReactNode;
  highlight: boolean;
  isClosed?: boolean;
  onShowDiffChange?(next: boolean): void;
  showDiff?: boolean;
  showVersionPicker: boolean;
  toolbar?: DocumentToolbar;
  version: number | undefined;
}): ReactNode {
  const [highlighted, setHighlighted] = useState(highlight);
  const navigate = useNavigate();
  const versions = [...(toolbar?.versions ?? artifact.versions)].sort(
    (left, right) => right.number - left.number
  );

  useEffect(() => {
    if (!highlight) {
      setHighlighted(false);
      return;
    }
    setHighlighted(true);
    const timeout = window.setTimeout(() => setHighlighted(false), 2000);
    return () => window.clearTimeout(timeout);
  }, [highlight]);

  return (
    <section className="space-y-4">
      <header
        className={`flex flex-wrap items-center justify-between gap-3 rounded-lg p-3 ${card} ${
          highlighted ? highlightRing : ""
        }`}
        data-testid="artifact-header"
      >
        <div className="min-w-0">
          {artifact.issue_key === null ? (
            <Link
              to={buildProjectPath({ kind: "documents", project: artifact.project })}
              className={`inline-flex min-h-11 items-center text-sm font-semibold ${linkText} ${linkHoverText}`}
            >
              Project {artifact.project}
            </Link>
          ) : null}
          <h2 className={`truncate text-lg font-semibold ${textPrimaryOnSurface}`}>
            {artifact.name}
          </h2>
          {artifact.approval === undefined ? null : (
            <div className="mt-1">
              <ApprovalChip artifact={artifact} variant="header" />
            </div>
          )}
        </div>
        {showVersionPicker || toolbar !== undefined ? (
          <div className="flex min-w-0 flex-wrap items-center gap-3">
            {showVersionPicker ? (
              <label
                className={`flex min-h-11 min-w-0 items-center gap-2 text-sm font-medium ${textSecondaryOnSurface}`}
              >
                Version
                <select
                  aria-label="Version"
                  className={`min-h-11 min-w-0 max-w-56 truncate rounded border px-2 py-2 font-normal ${inputClasses(true)}`}
                  onChange={(event) => {
                    const nextVersion = event.target.value;
                    navigate(
                      artifact.issue_key === null
                        ? buildProjectPath(
                            nextVersion === ""
                              ? {
                                  kind: "document",
                                  project: artifact.project,
                                  slug: artifact.slug,
                                }
                              : {
                                  kind: "document",
                                  project: artifact.project,
                                  slug: artifact.slug,
                                  version: Number(nextVersion),
                                }
                          )
                        : buildIssuePath(
                            nextVersion === ""
                              ? {
                                  key: artifact.issue_key,
                                  kind: "artifact",
                                  slug: artifact.slug,
                                }
                              : {
                                  key: artifact.issue_key,
                                  kind: "artifact",
                                  slug: artifact.slug,
                                  version: Number(nextVersion),
                                }
                          )
                    );
                  }}
                  value={version ?? ""}
                >
                  <option value="">Current</option>
                  {versions.map((item) => (
                    <option key={item.number} value={item.number}>
                      {versionLabel(item)}
                    </option>
                  ))}
                </select>
              </label>
            ) : null}
            {toolbar === undefined ? null : (
              <>
                <button
                  className={`shrink-0 rounded-lg px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
                  disabled={isClosed || toolbar.isNamingVersion}
                  onClick={toolbar.requestNamedVersion}
                  type="button"
                >
                  Name version
                </button>
                {version === undefined ? null : (
                  <button
                    className={`shrink-0 rounded-lg px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
                    onClick={() => onShowDiffChange?.(!showDiff)}
                    type="button"
                  >
                    {showDiff ? "Show version" : "Diff vs current"}
                  </button>
                )}
                <ConnectionDot connection={toolbar.connection} />
              </>
            )}
          </div>
        ) : null}
      </header>
      {children}
    </section>
  );
}

export function ArtifactBlobView({
  artifact,
  version,
}: {
  artifact: Artifact;
  version: number | undefined;
}): ReactNode {
  const latest = [...artifact.versions].sort((left, right) => right.number - left.number)[0];
  const selected =
    version === undefined ? latest : artifact.versions.find((item) => item.number === version);

  if (selected === undefined) {
    return version === undefined ? (
      <p className={`text-sm ${textMutedOnCanvas}`}>No versions have been uploaded.</p>
    ) : (
      <p className={`text-sm ${dangerText}`}>
        Version {version} is not available for this artifact.
      </p>
    );
  }

  const download = artifactVersionUrl(artifact.id, selected.number);
  return (
    <section aria-label={`${artifact.name} version ${selected.number}`} className="space-y-3">
      {artifact.kind === "image" ? (
        <img
          alt={`${artifact.name} version ${selected.number}`}
          className={`h-auto w-full rounded-lg border ${borderDefault}`}
          src={download}
        />
      ) : null}
      <dl className="grid grid-cols-2 gap-3 text-sm">
        <div>
          <dt className={textMutedOnCanvas}>Version</dt>
          <dd className={`font-medium ${textPrimaryOnCanvas}`}>{selected.number}</dd>
        </div>
        <div>
          <dt className={textMutedOnCanvas}>Size</dt>
          <dd className={`font-medium ${textPrimaryOnCanvas}`}>
            {formatArtifactBytes(selected.size)}
          </dd>
        </div>
      </dl>
      <p className={`text-sm ${textMutedOnCanvas}`}>
        Uploaded <Timestamp at={selected.created_at} />
      </p>
      <a
        className={`inline-flex min-h-11 items-center font-medium underline ${linkText} ${linkHoverText}`}
        download=""
        href={download}
      >
        Download version {selected.number}
      </a>
    </section>
  );
}
