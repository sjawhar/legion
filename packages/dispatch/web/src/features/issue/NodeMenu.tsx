import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import type { Artifact } from "../../api/types";
import {
  borderDefault,
  card,
  focusVisibleRing,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textSecondaryOnSurface,
} from "../../theme/classes";
import {
  ArtifactUploadStatus,
  CLOSED_ISSUE_UPLOAD_REFUSAL,
  useArtifactUpload,
} from "../artifacts/ArtifactUpload";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildReferencePath, documentRoute } from "../refs/routes";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/**
 * The node menu beside the presets (dispatch://INFRA-1196 R16,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t18`).
 *
 * There is no Artifacts preset. A document a block cites is a node of the tree under that
 * block, and opens from there; what is left over is here - a document nothing cites, and an
 * artifact that is no document and so is no node at all - each marked as cited by nothing, so
 * a reader sees why it is in a menu rather than in the issue's body. The upload is here too,
 * because adding a file is the one thing about an issue's files that no node answers for.
 *
 * The file input lives beside the menu rather than inside it, so a picker opened from a menu
 * item outlives the menu closing; a drop anywhere over the control uploads as well.
 */
export function NodeMenu({
  artifacts,
  isClosed,
  issueKey,
  onClose,
  onToggle,
  open,
}: {
  /** Every artifact of the issue no block of the tree cites (`issue-artifacts.ts`). */
  artifacts: readonly Artifact[];
  isClosed: boolean;
  issueKey: string;
  /** Closes the menu once an item has acted. */
  onClose: () => void;
  onToggle: () => void;
  open: boolean;
}): ReactNode {
  const navigate = useNavigate();
  const upload = useArtifactUpload(
    { issue: issueKey },
    { immediate: true, refusal: isClosed ? CLOSED_ISSUE_UPLOAD_REFUSAL : null }
  );
  return (
    <div className="relative shrink-0">
      <button
        aria-expanded={open}
        aria-haspopup="menu"
        className={NODE_MENU_BUTTON_CLASS}
        onClick={onToggle}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.other_nodes}
      </button>
      {/* The menu itself takes the focus, so the chord that opens it (`t a`) leaves the reader
          in it and the arrow keys reach its items. */}
      {open ? (
        <div className={MENU_CLASS} role="menu" tabIndex={-1}>
          {artifacts.map((artifact) => (
            // The item and the artifact's own copy control, side by side: a document the menu
            // lists is reachable here and nowhere else, so this is where it keeps the copy
            // control every other node of the page has (R18, c-nothing-lost).
            <div className="flex min-w-0 items-center gap-1" key={artifact.slug}>
              <button
                className={`min-w-0 flex-1 ${MENU_ITEM_CLASS}`}
                onClick={() => {
                  onClose();
                  navigate(buildReferencePath(documentRoute(artifact)));
                }}
                role="menuitem"
                type="button"
              >
                <span {...authorText}>{artifact.name}</span>{" "}
                <span {...pageChrome}>{CONTROL_LABELS.nothing_cites_it}</span>
              </button>
              <CopyRefButton route={documentRoute(artifact)} />
            </div>
          ))}
          <button
            className={MENU_ITEM_CLASS}
            onClick={() => upload.openPicker()}
            role="menuitem"
            type="button"
            {...pageChrome}
          >
            {CONTROL_LABELS.upload}
          </button>
        </div>
      ) : null}
      {/* The chooser the item above opens, beside the menu rather than inside it, so a pick
          outlives the menu closing. It is named apart from the Artifacts tab's own chooser,
          which a page drawing both would otherwise offer twice under one name. */}
      <input
        aria-label={CONTROL_LABELS.upload_file}
        className="sr-only left-0"
        disabled={upload.refusal !== null}
        onChange={upload.selectFile}
        ref={upload.fileInputRef}
        type="file"
      />
      <ArtifactUploadStatus upload={upload} />
    </div>
  );
}

/** The one shape both menus of the preset row wear: the presets' own below 1025 px, and the
 *  node menu's at every width. */
export const MENU_CLASS = `absolute left-0 top-full z-30 mt-1 flex w-56 flex-col gap-1 rounded-lg border p-2 shadow-lg ${borderDefault} ${card}`;

export const MENU_ITEM_CLASS = `min-h-11 rounded-lg px-2 py-2 text-left text-sm font-medium ${textSecondaryOnSurface}`;

/** The node menu's own control, which sits beside the preset row and outside it. */
const NODE_MENU_BUTTON_CLASS = `inline-flex min-h-11 items-center rounded-lg px-2 text-sm font-medium md:min-h-9 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${focusVisibleRing}`;
