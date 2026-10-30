import type { Actor, DeliveryCapability } from "../../api/types";
import type { RecordMarkPlacement } from "../doc/editor";

/** The composer's data model: where a draft goes, what kind it is, and what it carries. The
 *  composer (`MentionComposer.tsx`), the request it freezes (`send-request.ts`) and every host read
 *  these from here, so no module imports a type back from one that imports it. */

export type ComposerOwner =
  | { readonly kind: "issue"; readonly issueKey: string }
  | { readonly kind: "artifact"; readonly artifactId: string; readonly project: string }
  | { readonly kind: "session"; readonly sessionId: string };

export type ComposerKind = "ask" | "comment" | "suggestion";

export interface ComposerAnchor {
  readonly artifact: string;
  readonly mark_id: string;
  /** Which reading of `quote`, in which block, the editor wrote the mark over, as it reported
   *  when the selection bar made it. The composer keeps it because the editor keeps nothing a
   *  rebuilt document or a reconnect does not take with it, and the margin hands it back when
   *  the mark has to be written again (`DocumentBridge.remarkMark`). Absent where the editor had
   *  none to give, which leaves that recovery the quote alone. */
  readonly placement?: RecordMarkPlacement;
  readonly quote: string;
}

/** Why a compose's mark could not be written again after the server refused its send for a mark
 *  the document does not hold, in the reader's words, kept with the mark it answers: the composer
 *  shows it in place of the server's words, and a newer anchor leaves it behind. */
export interface AnchorRefusal {
  readonly markId: string;
  readonly text: string;
}

export interface ReplyTarget {
  /** The parent's author, which the Replying-to chip draws through `AgentName`. */
  readonly actor: Actor;
  readonly excerpt: string;
  readonly id: string;
  readonly parentKind: "comment" | "message";
  /** Canonical targets carried by a comment parent, prefilled as editable @ mentions. */
  readonly mentions?: readonly { readonly target: string; readonly title: string }[];
  /** Kept while old targeted-message threads remain readable; S2 writes comments only. */
  readonly thread?: {
    readonly delivery: DeliveryCapability;
    readonly target: string;
  };
  readonly to?: string;
}

/** One accepted mention: the target, and the span of the body that names it. */
export interface AcceptedMention {
  readonly end: number;
  readonly start: number;
  readonly target: string;
  readonly text: string;
}
