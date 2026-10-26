import type { Actor, DeliveryCapability } from "../../api/types";

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
  readonly quote: string;
}

export type ReplyTarget = (
  | {
      /** The parent's author, which the Replying-to chip draws through `AgentName`. */
      readonly actor: Actor;
      readonly author?: never;
    }
  | {
      readonly actor?: never;
      /** The parent's author as text, for a target that carries no `actor`: one that quotes no
       *  parent by author (a thread card's own reply). */
      readonly author: string;
    }
) & {
  readonly excerpt: string;
  readonly id: string;
  readonly parentKind: "comment" | "message";
  /** Canonical targets carried by a comment parent, prefilled as editable @ mentions. */
  readonly mentions?: readonly { readonly target: string; readonly title: string }[];
  /** Kept while old targeted-message threads remain readable; S2 writes comments only. */
  readonly thread?: {
    readonly delivery: DeliveryCapability;
    readonly target: string;
    readonly title: string;
  };
  readonly to?: string;
};

/** One accepted mention: the target, and the span of the body that names it. */
export interface AcceptedMention {
  readonly end: number;
  readonly start: number;
  readonly target: string;
  readonly text: string;
}
