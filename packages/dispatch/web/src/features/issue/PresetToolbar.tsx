import type { ReactNode } from "react";

import type { Artifact } from "../../api/types";
import { textMutedOnCanvas } from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import type { DocumentToolbar } from "../doc/ProofDocument";
import type { DispatchReferenceRoute } from "../refs/routes";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { SpecToolbar } from "./SpecToolbar";

/**
 * The toolbar of the preset whose document is open (dispatch://INFRA-1196 R16): which version
 * of it the page is reading, the control that gives its current text a version of its own, and
 * how it stands with its reviewers.
 *
 * It sits beside the preset row rather than above the document, because a preset draws its
 * document as nodes of the one tree and there is no second header to hang it from. The version
 * picker and the name-a-version control are the ones the document already owns
 * (`SpecToolbar.tsx`), so nothing a reader knew is renamed or moved; what this adds is the
 * approval beside them and the version the address pins, said in words rather than left for a
 * reader to read out of a closed select.
 */
export function PresetToolbar({
  artifact,
  isClosed,
  onShowDiffChange,
  onVersionChange,
  reference,
  showDiff,
  toolbar,
  version,
}: {
  artifact: Artifact;
  isClosed: boolean;
  onShowDiffChange: (next: boolean) => void;
  onVersionChange: (version: number | null) => void;
  reference: DispatchReferenceRoute;
  showDiff: boolean;
  toolbar: DocumentToolbar;
  /** The version the address pins (`?v=`), or undefined for the document as it stands. */
  version: number | undefined;
}): ReactNode {
  return (
    <div
      className="flex w-full min-w-0 flex-wrap items-center gap-2 md:ml-auto md:w-auto"
      data-testid="issue-preset-toolbar"
    >
      {version === undefined ? null : (
        <span className={`text-xs ${textMutedOnCanvas}`} {...pageChrome}>
          {CONTROL_LABELS.version} <span {...authorText}>{version}</span>
        </span>
      )}
      <SpecToolbar
        isClosed={isClosed}
        onShowDiffChange={onShowDiffChange}
        onVersionChange={onVersionChange}
        reference={reference}
        showDiff={showDiff}
        toolbar={toolbar}
        version={version}
      />
      <ApprovalChip artifact={artifact} />
    </div>
  );
}
