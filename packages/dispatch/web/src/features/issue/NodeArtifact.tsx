import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import {
  borderDefault,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { artifactVersionUrl, versionsNewestFirst } from "../artifacts/ArtifactHeader";
import { pictureClassName } from "../refs/DispatchPicture";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS, EMPTY_STATE_LINES } from "./page-words";

/** How tall an artifact's own text or picture is drawn inside a row, which scrolls past it: a
 *  node's body is read at a 660 px measure and the row it opens has to fit the screen after
 *  every move of the keyboard (R1, R9). It scrolls up and down and never sideways, and what
 *  scrolls is the author's own text, inside `[data-author-text]`, which is where R17's
 *  exception for a code block, a table and a picture reaches. A box that scrolls is a tab stop
 *  of Chromium's own, so it takes none: the row is the tree's one tab stop (R9). */
const ARTIFACT_MEASURE = "max-h-64";

/**
 * The body of an artifact's node: the document or picture itself, and the two things that stay
 * reachable from an artifact wherever it is drawn - every version it has, and the download of
 * one (dispatch://INFRA-1196 R16, `dispatch://INFRA-1196/artifact/plan-md#b-t18`).
 *
 * A document a block cites is a node under that block and opens from there, so this is what
 * "opens" means for it: its own text, as it stands. The text is the one read a document's node
 * makes (`GET /artifacts/{id}/text`, the key the editor's own admission read already uses), and
 * it is made only while the body is drawn, which is while the row has the focus or the reader
 * pinned it open. A node whose own editor the reader opened draws that editor instead
 * (`NodeRow`), so no row ever draws both.
 */
export function NodeArtifact({ artifact }: { artifact: Artifact }): ReactNode {
  const [showVersions, setShowVersions] = useState(false);
  const versions = versionsNewestFirst(artifact.versions);
  const latest = versions[0];
  return (
    <>
      {artifact.kind === "image" ? (
        <span className={`block ${ARTIFACT_MEASURE} overflow-y-auto`} tabIndex={-1} {...authorText}>
          {latest === undefined ? null : (
            <img
              alt={artifact.name}
              className={pictureClassName}
              src={artifactVersionUrl(artifact.id, latest.number)}
            />
          )}
        </span>
      ) : (
        <ArtifactText artifact={artifact} />
      )}
      <p className="flex min-w-0 flex-wrap items-baseline gap-2">
        {latest === undefined ? null : (
          <DownloadVersion artifact={artifact} version={latest.number} />
        )}
        <button
          aria-expanded={showVersions}
          className={`rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
          onClick={() => setShowVersions((shown) => !shown)}
          tabIndex={-1}
          type="button"
          {...pageChrome}
        >
          {CONTROL_LABELS.versions}
        </button>
      </p>
      {showVersions ? (
        <ul className="flex min-w-0 flex-wrap gap-2">
          {versions.map((version) => (
            <li key={version.number}>
              <DownloadVersion artifact={artifact} version={version.number} />
            </li>
          ))}
        </ul>
      ) : null}
    </>
  );
}

/** One version's download, named by the page and numbered by the artifact's own history. */
function DownloadVersion({
  artifact,
  version,
}: {
  artifact: Artifact;
  version: number;
}): ReactNode {
  return (
    <a
      className={`text-xs underline ${linkText} ${linkHoverText}`}
      download=""
      href={artifactVersionUrl(artifact.id, version)}
      tabIndex={-1}
      {...pageChrome}
    >
      {CONTROL_LABELS.download} <span {...authorText}>{version}</span>
    </a>
  );
}

/** A document node's own text, as the server holds it now. */
function ArtifactText({ artifact }: { artifact: Artifact }): ReactNode {
  const text = useQuery({
    queryKey: ["artifact", artifact.id, "text"],
    queryFn: () => api.getArtifactText(artifact.id),
  });
  if (text.data === undefined) {
    return (
      <p className={`text-xs ${textMutedOnCanvas}`} {...pageChrome}>
        {text.isError ? EMPTY_STATE_LINES.document_unread : EMPTY_STATE_LINES.document_reading}
      </p>
    );
  }
  return (
    <p
      className={`whitespace-pre-wrap break-words ${ARTIFACT_MEASURE} overflow-y-auto ${textSecondaryOnCanvas}`}
      tabIndex={-1}
      {...authorText}
    >
      {text.data.markdown}
    </p>
  );
}
