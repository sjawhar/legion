import { DOCUMENT_NEEDS_REBUILD_CLOSE_REASON } from "@legion/contracts";

import {
  dangerText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
} from "../../theme/classes";
import { refusedEditNotice } from "./connection";

/** What the rebuild a document refused as needing one asks before it runs: the rebuild closes the
 * document's room, and every browser connected to it drops the edits it has not sent. */
const NEEDS_REBUILD_CONFIRMATION =
  "Rebuild this document from its current text? Every browser connected to it, this one included, drops the edits it has not sent yet.";

export interface RefusedEditNoticeProps {
  /** The close reason the server refused this browser's edit with, which the notice is read from
   *  (`refusedEditNotice`) and which decides whether a rebuild is offered. */
  readonly reason: string;
  /** The document's own page, as against the Inbox's reading column: a reading column offers no
   *  rebuild, since it is not the document's page. */
  readonly page: boolean;
  /** Whether the rebuild this panel offered is out. */
  readonly rebuilding: boolean;
  /** How the rebuild this panel offered was refused, if it was. */
  readonly rebuildError: unknown;
  /** Takes the notice away: the person has read it. */
  readonly onDismiss: () => void;
  /** Rebuilds the document. Called only once the person has confirmed it. */
  readonly onRebuild: () => void;
}

/**
 * The panel the editor shows for an edit the server refused with `DOCUMENT_TOO_LARGE_CLOSE_CODE`:
 * the sentence for its reason (`refusedEditNotice`), a Dismiss, and, for the one reason a rebuild
 * takes - `DOCUMENT_NEEDS_REBUILD_CLOSE_REASON`, a document whose edit history has used up what
 * the server lets it scan - the offer to rebuild it. The other reasons offer none: a document
 * someone else's rebuild replaced (`DOCUMENT_REBUILT_CLOSE_REASON`) has just been rebuilt, and the
 * route answers a second rebuild of it `409`; an edit too large, too tangled or holding content
 * the document does not take is the edit's own refusal, which no rebuild mends.
 *
 * The rebuild route is human-only (`requireHuman`), and every person the dashboard signs in is a
 * user, so the offer goes to everyone the refusal reaches on the document's page. A refusal of the
 * rebuild itself shows beside its button.
 */
export function RefusedEditNotice({
  onDismiss,
  onRebuild,
  page,
  reason,
  rebuildError,
  rebuilding,
}: RefusedEditNoticeProps) {
  return (
    <div className="space-y-3" role="alert">
      <div className="flex flex-wrap items-center gap-3">
        <p className={dangerText}>{refusedEditNotice(reason)}</p>
        <button
          className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          onClick={onDismiss}
          type="button"
        >
          Dismiss
        </button>
      </div>
      {page && reason === DOCUMENT_NEEDS_REBUILD_CLOSE_REASON ? (
        <div className="flex flex-wrap items-center gap-3">
          <p>
            Rebuilding replaces the document's edit history with its current text, so it can take
            edits again.
          </p>
          <button
            className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
            disabled={rebuilding}
            onClick={() => {
              if (window.confirm(NEEDS_REBUILD_CONFIRMATION)) {
                onRebuild();
              }
            }}
            type="button"
          >
            {rebuilding ? "Rebuilding…" : "Rebuild this document"}
          </button>
          {rebuildError === undefined ? null : (
            <p className={dangerText}>
              {rebuildError instanceof Error
                ? rebuildError.message
                : "Could not rebuild this document."}
            </p>
          )}
        </div>
      ) : null}
    </div>
  );
}
