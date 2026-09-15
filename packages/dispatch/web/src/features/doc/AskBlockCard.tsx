import { type FormEvent, type ReactNode, useId, useState } from "react";
import { createPortal } from "react-dom";

import type { Ask } from "../../api/types";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import {
  askBlockPill,
  badgeBlocking,
  badgeLow,
  dangerText,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  surfaceBg,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { AskChoiceRow, AskRecordRow } from "../inbox/AskOptionRow";
import { URGENCY_LABELS } from "../inbox/ask-urgency";
import { actorLabel } from "../refs/actor";
import { CopyRefButton } from "../refs/CopyRefButton";
import { itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type AskBlockFacts, type AskBlockHost, askBlockFacts } from "./ask-block";

const PILL_CLASS = "rounded-full px-2 py-0.5 text-[10px] font-semibold tracking-wide uppercase";

export interface AskBlockCardProps {
  /** The indexed ask row for this block, once the server has created it. */
  ask: Ask | undefined;
  host: AskBlockHost;
  onAnswer(ask: Ask, selected: string[], text: string): void;
  /** The project document holding the block, for an ask that has no issue to be referenced
   *  under; an issue document's asks carry their `issue_key`. */
  owner: { project: string; slug: string } | undefined;
  /** Answering is in flight for this block. */
  pending: boolean;
  readOnly: boolean;
}

/** Everything a reader sees of a decision block besides its question text: rendered into the
 * node view's header and footer slots (see `AskBlockView`). The header names the urgency, who
 * asked and when, and who answered; the footer is the answer form while the decision is open and
 * the reader may answer, otherwise the recorded choice. */
export function AskBlockCard({
  ask,
  host,
  onAnswer,
  owner,
  pending,
  readOnly,
}: AskBlockCardProps): ReactNode {
  const facts = askBlockFacts(host.node);
  const malformed = facts.malformedReason !== undefined;
  const open = facts.state === "open";
  const answered = facts.state === "answered";
  const pill = askBlockPill[facts.urgency];
  // A block ask indexed without an actor (the server settled it with no pending author) still
  // shows when it was asked; "asked by" waits for a name.
  const asker = ask === undefined ? undefined : actorLabel(ask.author);
  const reference = ask === undefined ? undefined : itemRoute("ask", ask, owner);
  return (
    <>
      {createPortal(
        <>
          <header
            className={`not-prose mb-3 flex flex-wrap items-center gap-x-2 gap-y-1 text-sm ${textMutedOnSurfaceMuted}`}
          >
            {malformed ? (
              <span className={`${PILL_CLASS} ${badgeBlocking.bg} ${badgeBlocking.text}`}>
                Malformed
              </span>
            ) : (
              <span
                className={`${PILL_CLASS} ${pill.bg} ${pill.text}`}
                data-dispatch-ask-pill={facts.urgency}
              >
                {URGENCY_LABELS[facts.urgency]}
              </span>
            )}
            <strong className={textPrimaryOnSurface}>Decision</strong>
            {ask === undefined ? null : (
              <span className="inline-flex items-center gap-x-2" data-dispatch-ask-asked="">
                <span aria-hidden="true">·</span>
                <span>{asker === "" ? "asked" : `asked by ${asker}`}</span>
                {asker === "" ? null : <span aria-hidden="true">·</span>}
                <Timestamp at={ask.created_at} />
              </span>
            )}
            {reference === undefined ? null : <CopyRefButton route={reference} />}
            {answered ? (
              <span className="inline-flex items-center gap-x-2" data-dispatch-ask-answered="">
                <span aria-hidden="true">·</span>
                <span>Answered by {facts.answeredBy}</span>
                {facts.answeredAt === undefined ? null : (
                  <>
                    <span aria-hidden="true">·</span>
                    <Timestamp at={facts.answeredAt} />
                  </>
                )}
              </span>
            ) : null}
            {!malformed && !open && !answered ? (
              <span className={`${PILL_CLASS} ${badgeLow.bg} ${badgeLow.text}`}>
                {facts.state === "resolved" ? "Resolved" : "Closed"}
              </span>
            ) : null}
          </header>
          {malformed ? (
            <p className={`not-prose mb-2 text-sm font-medium ${dangerText}`}>
              {facts.malformedReason}
            </p>
          ) : null}
        </>,
        host.header
      )}
      {createPortal(
        malformed ? (
          <p className={`not-prose mt-3 text-sm ${textMutedOnSurfaceMuted}`}>
            Fix the block text; the decision re-activates once it parses.
          </p>
        ) : open && !readOnly ? (
          <AnswerForm ask={ask} facts={facts} onAnswer={onAnswer} pending={pending} />
        ) : (
          <AnswerRecord facts={facts} />
        ),
        host.footer
      )}
    </>
  );
}

function AnswerForm({
  ask,
  facts,
  onAnswer,
  pending,
}: {
  ask: Ask | undefined;
  facts: AskBlockFacts;
  onAnswer(ask: Ask, selected: string[], text: string): void;
  pending: boolean;
}): ReactNode {
  const name = `${useId()}-selected`;
  const [selected, setSelected] = useState<string[]>([]);
  const [other, setOther] = useState(false);
  const [text, setText] = useState("");
  const hasOptions = facts.options.length > 0;
  const trimmed = text.trim();
  // Same rule as the Inbox card (`useAskAnswerForm`): a real option answers on its own; "Other"
  // needs words, and so does an option-less decision. With multiple choice, Other sits beside the
  // options rather than replacing them.
  const canAnswer = hasOptions ? (other ? trimmed !== "" : selected.length > 0) : trimmed !== "";
  // The form stays disabled until the server has indexed the block into an ask that can take
  // the answer.
  const disabled = ask === undefined || pending;
  const choose = (label: string) => {
    if (facts.multiple) {
      setSelected((current) =>
        current.includes(label) ? current.filter((item) => item !== label) : [...current, label]
      );
      return;
    }
    setSelected([label]);
    setOther(false);
  };
  const toggleOther = () => {
    if (facts.multiple) {
      setOther((current) => !current);
      return;
    }
    setSelected([]);
    setOther(true);
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (ask === undefined || !canAnswer || pending) {
      return;
    }
    onAnswer(ask, selected, trimmed);
  };
  return (
    <form className="not-prose mt-3" data-dispatch-ask-form={facts.blockId} onSubmit={submit}>
      <fieldset className="space-y-2" disabled={disabled}>
        <legend className="sr-only">Your answer</legend>
        {facts.options.map((option) => (
          <AskChoiceRow
            checked={selected.includes(option.label)}
            className={`min-h-11 ${surfaceBg}`}
            key={option.label}
            multiple={facts.multiple}
            name={name}
            onChange={() => choose(option.label)}
            option={option}
          />
        ))}
        {hasOptions ? (
          <AskChoiceRow
            checked={other}
            className={`min-h-11 ${surfaceBg}`}
            multiple={facts.multiple}
            name={name}
            onChange={toggleOther}
            option={{ label: "Other" }}
          />
        ) : null}
        <label className="block">
          <span className="sr-only">Your answer</span>
          <textarea
            className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${textPrimaryOnSurface} ${inputClasses(true)}`}
            name="answer"
            onChange={(event) => setText(event.target.value)}
            onKeyDown={(event) => submitOnModifiedEnter(event)}
            placeholder={
              hasOptions ? "Add a note, or answer in your own words" : "Answer in your own words"
            }
            rows={2}
            value={text}
          />
        </label>
        <button
          className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
          disabled={disabled || !canAnswer}
          type="submit"
        >
          {pending ? "Answering…" : "Answer"}
        </button>
      </fieldset>
    </form>
  );
}

/** The decision's recorded outcome: its options with the chosen ones ticked, and the note. Also
 * what a read-only surface shows for a still-open decision (options, nothing chosen). */
function AnswerRecord({ facts }: { facts: AskBlockFacts }): ReactNode {
  const otherText =
    facts.state === "answered" && facts.options.length > 0 && facts.selected.length === 0
      ? facts.answerText
      : "";
  return (
    <div className="not-prose mt-3 space-y-2" data-dispatch-ask-record="">
      {facts.options.length === 0 ? null : (
        <ul aria-label="Options" className="m-0 list-none space-y-1 p-0">
          {facts.options.map((option) => (
            <AskRecordRow
              className={surfaceBg}
              key={option.label}
              option={option}
              selected={facts.selected.includes(option.label)}
            />
          ))}
        </ul>
      )}
      {otherText === "" ? null : (
        <p className={`text-sm font-medium ${textPrimaryOnSurface}`}>Other</p>
      )}
      {facts.answerText === "" ? null : (
        <p className={`text-sm ${textMutedOnSurfaceMuted}`} data-dispatch-ask-answer="">
          {facts.answerText}
        </p>
      )}
    </div>
  );
}
