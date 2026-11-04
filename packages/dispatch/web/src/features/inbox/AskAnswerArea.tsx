import { DISPATCH_BODY_MAX } from "@legion/contracts/dispatch-tools";
import {
  type Dispatch,
  type ReactNode,
  type RefObject,
  type SetStateAction,
  useId,
  useRef,
} from "react";

import type { ArtifactOwner } from "../../api/client";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { textMutedOnSurface } from "../../theme/classes";
import { appendToDraft, DraftUploadStatus, useDraftUpload } from "../artifacts/ArtifactUpload";
import {
  MentionField,
  type MentionFieldHandle,
  useMentionOptions,
} from "../conversation/MentionField";
import type { Draft } from "./draft-store";

/** What the buttons under the area are given: the id of the line already showing each refusal the
 *  host named, and whether a file the field took is still uploading. */
export interface AskAnswerActions {
  readonly reasonId: Readonly<Record<string, string>>;
  readonly uploading: boolean;
}

/**
 * The one answer area of an ask: the Conversation tab's own body field and `@` picker
 * (`features/conversation/MentionField.tsx`), the draft behind it, the uploads a file pasted or
 * dropped into it starts, and the line that says why a send refuses. Its two hosts differ only in
 * what they send it with, which they pass as `actions`: the open card's Answer and Ask back
 * (`AskCard.tsx`, which answers through `useAskAnswerForm`), and the reply box under an answered
 * ask (`AskThread.tsx`). The draft itself is the host's - the ask's answer draft or its reply
 * draft (`answer-drafts.ts`, `reply-drafts.ts`) - because the host is what sends it.
 *
 * A picked session is stored as a `session:` mention on the draft and goes to the wire as
 * `CreateCommentInput.mentions`, so the comment reaches that session. An answer carries no
 * mentions at all, so a host whose send is an answer refuses the draft while one survives
 * (`ANSWER_MENTION_REFUSAL`) rather than dropping it silently.
 */
export function AskAnswerArea({
  actions,
  disabled = false,
  draft,
  fieldRef,
  handle,
  label,
  labelHidden = false,
  onDraftChange,
  placeholder,
  refusals,
  uploadOwner,
}: {
  readonly actions: (state: AskAnswerActions) => ReactNode;
  /** The field takes no typing: this ask's own send is out, and the draft on the wire is not the
   *  reader's to write over. */
  readonly disabled?: boolean;
  readonly draft: Draft;
  /** The host's handle on the field, where it moves focus into the box itself (`AskCard` lands
   *  the reader in the reason an approval demands). */
  readonly fieldRef?: RefObject<MentionFieldHandle | null>;
  /** A bare `data-` attribute for the textarea (`MentionField`'s `handle`). */
  readonly handle?: string;
  readonly label: string;
  readonly labelHidden?: boolean;
  readonly onDraftChange: Dispatch<SetStateAction<Draft>>;
  readonly placeholder?: string;
  /** Why each of the host's buttons refuses the draft as it stands, by the name the host knows it
   *  by: each reason is shown on one line under the field, and `actions` is handed that line's id
   *  for the button's `refusalShownBy`. */
  readonly refusals: Readonly<Record<string, string | null>>;
  /** Where a file pasted or dropped into the box goes: the ask's issue, or its document's
   *  project. Without one the field takes a paste as text. */
  readonly uploadOwner?: ArtifactOwner;
}): ReactNode {
  // Everything the `@` picker needs, from the one place both its hosts take it
  // (`useMentionOptions`): the registry is kept fresh through the shell's one poller while the
  // picker is open and read again the moment it opens, exactly as the Conversation tab's
  // composer keeps it. The sessions `@` lists are the same sessions, read the same way.
  const { onPickerOpenChange, options } = useMentionOptions(true);
  const ownFieldRef = useRef<MentionFieldHandle>(null);
  const field = fieldRef ?? ownFieldRef;
  const upload = useDraftUpload((text) =>
    onDraftChange((current) => ({ ...current, text: appendToDraft(current.text, text) }))
  );
  const uploading = upload.pending > 0;
  // One area can be on the page twice (a decision block and the margin sheet, a card and its
  // drawer), so the line each button names is this instance's.
  const reasonBase = useId();
  const reasonId: Record<string, string> = {};
  for (const name of Object.keys(refusals)) reasonId[name] = `${reasonBase}-${name}`;

  return (
    <div className="space-y-2">
      <MentionField
        body={draft.text}
        disabled={disabled}
        fieldRef={field}
        handle={handle}
        label={label}
        labelHidden={labelHidden}
        maxLength={DISPATCH_BODY_MAX}
        mentions={draft.mentions}
        onChange={({ body, mentions }) => onDraftChange({ mentions, text: body })}
        onDrop={uploadOwner === undefined ? undefined : (event) => upload.drop(event, uploadOwner)}
        // A draft sent before its upload lands would go without the picture; the buttons say the
        // upload is what holds them (`busy`), and this key says nothing at all.
        onKeyDown={(event) => submitOnModifiedEnter(event, { disabled: uploading })}
        onPaste={
          uploadOwner === undefined ? undefined : (event) => upload.paste(event, uploadOwner)
        }
        onPickerOpenChange={onPickerOpenChange}
        options={options}
        placeholder={placeholder}
      />
      <DraftUploadStatus upload={upload} />
      {/* The reason a send refuses, on screen beside the draft it was refused over, and the
          description its button carries (`RefusableButton`'s `refusalShownBy`). */}
      {Object.entries(refusals).map(([name, reason]) =>
        reason === null ? null : (
          <p className={`text-xs ${textMutedOnSurface}`} id={reasonId[name]} key={name}>
            {reason}
          </p>
        )
      )}
      {actions({ reasonId, uploading })}
    </div>
  );
}
