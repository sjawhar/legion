import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useId, useState } from "react";

import { api } from "../../api/client";
import type { Artifact, Decision, IssueDecisions } from "../../api/types";
import {
  borderDefault,
  dangerText,
  inputClasses,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/**
 * What a reader does with a decision, on the decision's own node (dispatch://INFRA-1196 R7, the
 * spec's "Decisions tab ... Keep and Change" row, `plan-md#b-t18a`).
 *
 * The old Decisions tab drew four sections of cards in a panel of its own and put Keep and
 * Change on the cards of one of them; the tree draws each decision as a node, so the two
 * controls are in the node's body, where a reader is already reading the decision. The body is
 * the focused row's and a row the reader pinned open, so the page offers one Keep and one
 * Change at a time, on the decision in front of the reader.
 *
 * Both writes are the ones that tab made, unchanged: Keep is
 * `POST /artifacts/{id}/blocks/{block}/keep` carrying the sum of the text this node drew, so a
 * decision the agent changed since is refused rather than agreed with, and Change is a comment
 * anchored to the decision's first paragraph. They are read from the decisions read the tree is
 * already built from (`["decisions", <key>]`), so the node makes no read of its own.
 */
export function DecisionActions({
  document,
  isClosed,
  nodeReference,
}: {
  /** The decisions document this node is a block of, which the Keep writes against;
   *  `undefined` for a decision whose document the issue read does not carry. */
  document: Artifact | undefined;
  isClosed: boolean;
  /** The node's own `dispatch://` address, which is the decision's own (`Decision.ref`). */
  nodeReference: string | undefined;
}): ReactNode {
  const issueKey = document?.issue_key ?? "";
  const decisions = useQuery(issueDecisionsQuery(issueKey, issueKey !== ""));
  const decision = decisions.data?.decisions.find((row) => row.ref === nodeReference);
  if (decision === undefined || document === undefined || isClosed) return null;
  return <DecisionControls decision={decision} documentId={document.id} issueKey={issueKey} />;
}

function DecisionControls({
  decision,
  documentId,
  issueKey,
}: {
  decision: Decision;
  documentId: string;
  issueKey: string;
}): ReactNode {
  const queryClient = useQueryClient();
  const [changing, setChanging] = useState(false);
  const quote = firstParagraph(decision.text);
  const keep = useKeyedWrite({
    inline: true,
    label: () => `${CONTROL_LABELS.keep_decision} ${decision.heading}`,
    // The sum of the text this node showed: a decision the agent changed since is refused
    // (`409 DECISION_CHANGED`), so a Keep never stands over words the reader did not see.
    write: (id: string, bounds) =>
      api.keepDecision(id, decision.block_id, decision.text_sha256, bounds),
    onSuccess: (kept) => {
      if (kept === null) {
        void queryClient.invalidateQueries({ exact: true, queryKey: ["decisions", issueKey] });
        return;
      }
      queryClient.setQueryData<IssueDecisions>(["decisions", issueKey], (view) =>
        view === undefined
          ? view
          : {
              ...view,
              // The Keep is of the text this node showed, which the server took, so it applies
              // to the row that still holds that text.
              decisions: view.decisions.map((row) =>
                row.block_id === kept.block_id && row.text_sha256 === decision.text_sha256
                  ? { ...row, kept, waiting_review: false }
                  : row
              ),
            }
      );
    },
  });
  return (
    <>
      <p className="flex min-w-0 flex-wrap items-baseline gap-2">
        {decision.waiting_review ? (
          <button
            className={`rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
            disabled={keep.isPending}
            onClick={() => keep.mutate(documentId)}
            tabIndex={-1}
            type="button"
            {...pageChrome}
          >
            {CONTROL_LABELS.keep_decision}
          </button>
        ) : null}
        {quote === undefined ? null : (
          <button
            aria-expanded={changing}
            className={`rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
            onClick={() => setChanging(!changing)}
            tabIndex={-1}
            type="button"
            {...pageChrome}
          >
            {CONTROL_LABELS.change_decision}
          </button>
        )}
      </p>
      {keep.error === null ? null : (
        <p className={`text-xs ${dangerText}`} role="alert" {...pageChrome}>
          {keep.error.message}
        </p>
      )}
      {changing && quote !== undefined ? (
        <ChangeComposer
          decision={decision}
          documentId={documentId}
          issueKey={issueKey}
          onDone={() => setChanging(false)}
          quote={quote}
        />
      ) : null}
    </>
  );
}

/** The change itself: a comment of the issue's, anchored to the decision's own words, which is
 *  the write the Decisions tab made. */
function ChangeComposer({
  decision,
  documentId,
  issueKey,
  onDone,
  quote,
}: {
  decision: Decision;
  documentId: string;
  issueKey: string;
  onDone: () => void;
  quote: string;
}): ReactNode {
  const [body, setBody] = useState("");
  const fieldId = useId();
  const send = useKeyedWrite({
    inline: true,
    label: () => `${CONTROL_LABELS.change_decision} ${decision.heading}`,
    // A comment on an issue's document is the issue's own write: the artifact route refuses it,
    // naming the issue route (`send-request.ts` follows the same rule for a thread's replies).
    write: (input: { body: string; quote: string }, bounds) =>
      api.createComment(
        issueKey,
        { anchor: { artifact: documentId, quote: input.quote }, body: input.body },
        bounds
      ),
    onSuccess: () => {
      setBody("");
      onDone();
    },
  });
  return (
    <form
      aria-label={`${CONTROL_LABELS.change_decision} ${decision.heading}`}
      className="flex min-w-0 flex-col gap-1"
      onSubmit={(event) => {
        event.preventDefault();
        if (body.trim() !== "") send.mutate({ body, quote });
      }}
    >
      <blockquote
        className={`border-l-2 pl-2 text-xs break-words ${borderDefault} ${textSecondaryOnCanvas}`}
        data-testid="decision-quote"
        {...authorText}
      >
        {quote}
      </blockquote>
      <label className={`text-xs ${textMutedOnCanvas}`} htmlFor={fieldId} {...pageChrome}>
        {CONTROL_LABELS.your_change}
      </label>
      <textarea
        className={`block w-full min-w-0 rounded-lg border px-2 py-1 text-sm outline-none ${inputClasses(true)}`}
        id={fieldId}
        onChange={(event) => setBody(event.target.value)}
        rows={3}
        value={body}
      />
      {send.error === null ? null : (
        <p className={`text-xs ${dangerText}`} role="alert" {...pageChrome}>
          {send.error.message}
        </p>
      )}
      <p className="flex min-w-0 flex-wrap items-baseline gap-2">
        <button
          className={`rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
          disabled={send.isPending || body.trim() === ""}
          type="submit"
          {...pageChrome}
        >
          {CONTROL_LABELS.send_change}
        </button>
        <button
          className={`rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
          onClick={onDone}
          tabIndex={-1}
          type="button"
          {...pageChrome}
        >
          {CONTROL_LABELS.cancel}
        </button>
      </p>
    </form>
  );
}

/** The decision's first paragraph below its heading, which is what a change is anchored to.
 *  Undefined for a decision whose heading is all it holds. */
function firstParagraph(text: string): string | undefined {
  return text
    .split(/\n\s*\n/)
    .map((paragraph) => paragraph.trim())
    .find((paragraph) => paragraph !== "" && !paragraph.startsWith("#"));
}
