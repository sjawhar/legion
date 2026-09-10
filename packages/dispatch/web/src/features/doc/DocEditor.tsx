import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, Comment, Version } from "../../api/types";
import {
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  card,
  dangerText,
  inlineWarningText,
  inputClasses,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useMargin } from "../margin/Margin";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { DocView, type DocViewHighlight } from "./DocView";
import { VersionDiff } from "./VersionDiff";

interface DocEditorProps {
  artifact: Artifact;
  commentId?: string;
  highlight?: DocViewHighlight;
  isClosed: boolean;
  issueKey?: string;
  onVersionChange?: (version: number | null) => void;
  selectedVersion?: number | null;
}

function historicalHighlight(
  markdown: string,
  fallback: DocViewHighlight | undefined,
  anchor: Comment["anchor"] | undefined
): DocViewHighlight | undefined {
  if (anchor === null || anchor === undefined || anchor.orphaned) {
    return fallback;
  }
  const from = markdown.indexOf(anchor.quote);
  return from === -1 || markdown.indexOf(anchor.quote, from + 1) !== -1
    ? fallback
    : { from, to: from + anchor.quote.length };
}

export function DocEditor(props: DocEditorProps): ReactNode {
  return <DocEditorContent key={props.artifact.id} {...props} />;
}

function DocEditorContent({
  artifact,
  commentId,
  highlight,
  isClosed,
  issueKey,
  onVersionChange,
  selectedVersion: controlledSelectedVersion,
}: DocEditorProps): ReactNode {
  const { setDocumentText, setSelection } = useMargin();
  const queryClient = useQueryClient();
  const [localSelectedVersion, setLocalSelectedVersion] = useState<number | null>(null);
  const selectedVersion =
    onVersionChange === undefined ? localSelectedVersion : (controlledSelectedVersion ?? null);
  const setSelectedVersion = (version: number | null) => {
    if (onVersionChange === undefined) {
      setLocalSelectedVersion(version);
      return;
    }
    onVersionChange(version);
  };
  const [showDiff, setShowDiff] = useState(false);

  const artifactQuery = useQuery({
    queryKey: ["artifact", artifact.id],
    queryFn: () => api.getArtifact(artifact.id),
  });
  const liveTextQuery = useQuery({
    queryKey: ["artifact", artifact.id, "text"],
    queryFn: () => api.getArtifactText(artifact.id),
  });
  const versionTextQuery = useQuery({
    enabled: selectedVersion !== null,
    queryKey: ["artifact", artifact.id, "version", selectedVersion],
    queryFn: () => api.getArtifactVersion(artifact.id, selectedVersion ?? 0),
  });
  const comments = useQuery({
    enabled: issueKey !== undefined && commentId !== undefined,
    queryKey: ["comments", issueKey, artifact.id, commentId],
    queryFn: () => api.listComments(issueKey ?? "", artifact.id),
  });
  const nameVersion = useMutation({
    mutationFn: (summary: string) => api.createArtifactVersion(artifact.id, { summary }),
    onSuccess: (version) => {
      queryClient.setQueryData<Artifact>(["artifact", artifact.id], (current) => {
        const source = current ?? artifact;
        return {
          ...source,
          versions: [...source.versions.filter(({ number }) => number !== version.number), version],
        };
      });
      setSelectedVersion(version.number);
      setShowDiff(false);
    },
  });
  const liveMarkdown = liveTextQuery.data?.markdown ?? "";
  const selectedMarkdown =
    versionTextQuery.data !== undefined && "markdown" in versionTextQuery.data
      ? versionTextQuery.data.markdown
      : undefined;
  const versions = [...(artifactQuery.data?.versions ?? artifact.versions)].sort(
    (left, right) => right.number - left.number
  );
  const selectedVersionMeta =
    selectedVersion === null
      ? undefined
      : versions.find((version) => version.number === selectedVersion);
  const selectedAnchor = comments.data?.find((comment) => comment.id === commentId)?.anchor;
  const selectedHighlight =
    selectedVersion === null || selectedMarkdown === undefined
      ? undefined
      : historicalHighlight(
          selectedMarkdown,
          undefined,
          selectedAnchor?.version === selectedVersion ? selectedAnchor : undefined
        );
  const selectedQuoteMissing =
    selectedVersion !== null &&
    selectedAnchor !== null &&
    selectedAnchor !== undefined &&
    selectedAnchor.version === selectedVersion &&
    selectedHighlight === undefined;
  const liveHighlight = historicalHighlight(liveMarkdown, highlight, selectedAnchor);

  useEffect(() => setDocumentText(liveMarkdown), [liveMarkdown, setDocumentText]);

  const selectVersion = (value: string) => {
    setSelectedVersion(value === "" ? null : Number(value));
    setShowDiff(false);
  };
  const requestNamedVersion = () => {
    const summary = window.prompt("What changed in this version?");
    if (summary?.trim()) {
      nameVersion.mutate(summary.trim());
    }
  };

  return (
    <section aria-label="Document editor" className="space-y-4">
      <div
        className={`flex flex-wrap items-center justify-between gap-3 rounded-lg border p-3 ${card}`}
      >
        <div className="flex flex-wrap items-center gap-2">
          <button
            className={`rounded border px-3 py-1.5 text-sm font-medium disabled:cursor-not-allowed ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
            disabled={isClosed || nameVersion.isPending}
            onClick={requestNamedVersion}
            type="button"
          >
            Name version
          </button>
          {selectedVersion === null ? null : (
            <button
              className={`rounded border px-3 py-1.5 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              onClick={() => setShowDiff((visible) => !visible)}
              type="button"
            >
              {showDiff ? "Show version" : "Diff vs current"}
            </button>
          )}
        </div>
        <label
          className={`flex min-w-0 items-center text-sm font-medium ${textSecondaryOnSurface}`}
        >
          Version
          <select
            className={`ml-2 min-w-0 max-w-56 truncate rounded px-2 py-1 font-normal ${inputClasses(true)}`}
            onChange={(event) => selectVersion(event.target.value)}
            value={selectedVersion ?? ""}
          >
            <option value="">Current</option>
            {versions.map((version: Version) => (
              <option key={version.number} value={version.number}>
                Version {version.number}
                {version.named && version.summary !== null ? ` — ${version.summary}` : ""}
              </option>
            ))}
          </select>
        </label>
      </div>
      {isClosed ? (
        <p
          className={`rounded-lg border p-3 text-sm ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
        >
          This issue is closed. Its document is read-only.
        </p>
      ) : null}
      {nameVersion.isError ? (
        <p className={`text-sm ${dangerText}`} role="alert">
          Could not name this version.
        </p>
      ) : null}
      {selectedVersion !== null && versionTextQuery.isError ? (
        <section aria-label={`Document version ${selectedVersion}`} className="space-y-3">
          <p className={`text-sm ${dangerText}`}>
            No version {selectedVersion} of {artifact.name}.
          </p>
          <Link
            className={`font-medium underline ${linkText} ${linkHoverText}`}
            to={
              artifact.primary
                ? buildIssuePath({ key: artifact.issue_key, kind: "spec" })
                : buildIssuePath({
                    key: artifact.issue_key,
                    kind: "artifact",
                    slug: artifact.slug,
                  })
            }
          >
            View current version
          </Link>
        </section>
      ) : selectedVersion !== null && selectedMarkdown === undefined ? (
        <p className={`text-sm ${textMutedOnSurface}`}>Loading version…</p>
      ) : selectedVersion !== null && selectedMarkdown !== undefined ? (
        showDiff ? (
          <VersionDiff after={liveMarkdown} before={selectedMarkdown} />
        ) : (
          <section aria-label={`Document version ${selectedVersion}`} className="space-y-3">
            <h2 className={`text-lg font-semibold ${textPrimaryOnSurface}`}>
              Version {selectedVersion}
              {selectedVersionMeta?.created_at === undefined ? null : (
                <span className={`ml-2 text-sm font-normal ${textMutedOnSurface}`}>
                  <Timestamp at={selectedVersionMeta.created_at} />
                </span>
              )}
            </h2>
            {selectedQuoteMissing ? (
              <p className={`mb-2 text-sm ${inlineWarningText}`} role="status">
                Text changed. The selected range no longer exists in this document.
              </p>
            ) : null}
            <div data-testid="version-view">
              <DocView highlight={selectedHighlight} markdown={selectedMarkdown} />
            </div>
          </section>
        )
      ) : (
        <DocView
          highlight={liveHighlight}
          markdown={liveMarkdown}
          onSelectionChange={(next) => {
            setSelection(
              next === undefined
                ? undefined
                : {
                    ...next,
                    artifact: artifact.id,
                    artifactId: artifact.id,
                    canSuggest: true,
                  }
            );
          }}
        />
      )}
    </section>
  );
}
