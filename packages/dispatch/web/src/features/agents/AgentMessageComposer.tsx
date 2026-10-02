import { type MutationKey, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";

import { api } from "../../api/client";
import { agentMessagesQuery } from "../../api/queries";
import type { Agent } from "../../api/types";
import {
  borderDefault,
  dangerText,
  inputClasses,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { MentionComposer, type ReplyTarget } from "../conversation/MentionComposer";
import { focusOnDocument } from "../shell/roving";
import { AGENT_ROW_SELECTOR, ISSUE_PICKER_SELECTOR } from "./keyboard";

/** A reply the agent composer answers: the quoted message and the conversation it belongs to
 *  (an issue, or none), which decides the route the reply is created through. */
export interface AgentReply {
  readonly issueKey: string | null;
  readonly target: ReplyTarget;
}

/** Agent rows mounted this page load, so each mount's send has a name no other mount shares. */
let agentRowMounts = 0;

/** The name of an agent row's send. The row reads it (`useIsMutating`) to hold what a message on
 *  its way has already fixed - the picker and the Reply buttons - and the composer names its send
 *  with it, so the two keys cannot drift apart. It belongs to one mount of the row: a row that
 *  unmounts mid-send (its session left the registry, or the reader left the page) comes back with
 *  a fresh composer, which holds nothing for a send it did not make. */
export function useAgentSendKey(sessionID: string): MutationKey {
  const [mount] = useState(() => {
    agentRowMounts += 1;
    return agentRowMounts;
  });
  return useMemo(() => ["agent-composer", sessionID, mount], [sessionID, mount]);
}

/**
 * An agent row's composer: the issue picker and the `MentionComposer` it addresses. The row keeps
 * it mounted from its first open on, collapsed or moved, so the draft, the send in flight, its
 * refusal and its uploads each live in one place - here and in `MentionComposer` - and never have
 * to be carried from one instance to another.
 */
export function AgentMessageComposer({
  agent,
  onCancelReply,
  onClose,
  replyTo,
  sending,
  sendKey,
}: {
  agent: Agent;
  onCancelReply: () => void;
  /** One level out of the composer: the row it belongs to takes focus. The composer calls it on
   *  Escape from an untouched draft and on Discard - and also right after a successful send,
   *  which is NOT one level out; that case is filtered below. */
  onClose: () => void;
  replyTo: AgentReply | null;
  /** Whether this row's send is in flight (`sendKey`). The message is addressed by then, so the
   *  picker holds until it lands. */
  sending: boolean;
  /** The row's `useAgentSendKey`, which names the composer's send. */
  sendKey: MutationKey;
}): ReactNode {
  const queryClient = useQueryClient();
  const [issueKey, setIssueKey] = useState("");
  const [issuePickerOpen, setIssuePickerOpen] = useState(false);
  // `MentionComposer` calls `onSent` and then `onClose` on a successful send (its save's
  // `onSuccess`), and a reader who has just sent a message is still writing to this agent: moving
  // focus to the row would turn their next letters into `x` / `i` / `Shift+P` shortcuts. The flag
  // is set on the way past `onSent` and consumed by the `onClose` that follows it.
  const sentJustNow = useRef(false);
  const box = useRef<HTMLDivElement>(null);
  // `MentionComposer` disables its control fieldset while a send is in flight, including this
  // textarea. The browser then hands focus back to the document, so focus returns the moment React
  // re-enables the field - watched, rather than guessed at with a frame or timer, because the
  // write's latency is the server's.
  const refocusWatcher = useRef<MutationObserver | null>(null);
  useEffect(() => () => refocusWatcher.current?.disconnect(), []);
  /** Only the focus the disable took is the composer's to give back: through the whole round trip
   *  it sits on the document, so a reader who has clicked something else in the meantime keeps
   *  where they went - otherwise the next keys, `Ctrl+Enter` included, would land in the composer
   *  they have already sent from, addressed to another agent. */
  const refocusComposer = () => {
    const field = box.current?.querySelector("textarea");
    if (field === null || field === undefined) return;
    const takeBack = () => {
      if (focusOnDocument()) field.focus();
    };
    refocusWatcher.current?.disconnect();
    if (!field.matches(":disabled")) {
      takeBack();
      return;
    }
    const watcher = new MutationObserver(() => {
      if (field.matches(":disabled")) return;
      watcher.disconnect();
      takeBack();
    });
    watcher.observe(field.closest("fieldset") ?? field, { attributeFilter: ["disabled"] });
    refocusWatcher.current = watcher;
  };
  /** What the picker's selection reads while it is open, which is the reader's until they commit
   *  it: the select's own keys move it, `Enter`, or a pick made with the pointer or in the native
   *  popup, takes it, and leaving the select without committing puts it back on `issueKey`. */
  const [pendingIssue, setPendingIssue] = useState(issueKey);
  /** Whether the change arriving now is a key on the select stepping its selection, which only
   *  moves it: `Enter` is the pick. The test is the task the change arrives in, not the key.
   *  Chromium, Firefox and WebKit all step a closed select from the key event's own default
   *  action - the arrows, `Home`/`End` and the page keys from `keydown`, type-ahead from
   *  `keypress` - and dispatch `change` in that same task. A key that opens the native popup
   *  instead (the arrows on macOS; `Alt+ArrowDown` in Chromium and Firefox on Linux) steps
   *  nothing, and the pick then made in the popup arrives in a later task, as a pointer's does:
   *  that is a pick made, and it commits at once. So each key on the select marks the flag and
   *  the next task clears it. */
  const movedByKeyboard = useRef(false);
  const markKeyStep = () => {
    movedByKeyboard.current = true;
    setTimeout(() => {
      movedByKeyboard.current = false;
    }, 0);
  };
  /** Bumped by every commit, the issue changed or not, since every commit unmounts the select the
   *  reader is in. The hand-off keys on it rather than on `issueKey`, which re-confirming the
   *  issue already held leaves alone. */
  const [commits, setCommits] = useState(0);
  const commitIssue = (value: string) => {
    setIssueKey(value);
    setIssuePickerOpen(false);
    setCommits((count) => count + 1);
  };
  // A layout effect, so the frame the commit paints already has the field focused rather than
  // the document: the reader's next keystroke is the message, whichever hand made the pick.
  useLayoutEffect(() => {
    if (commits === 0) return;
    box.current?.querySelector("textarea")?.focus();
  }, [commits]);
  const issues = useQuery({
    enabled: issuePickerOpen,
    queryFn: () => api.listIssues({ open: true }),
    queryKey: ["agents", "issue-picker"],
  });
  /** The committed issue, once the open list has come back without it: closed since it was
   *  picked. The select and the toggle keep naming it, marked, until the reader picks another -
   *  the issue header's Status select keeps a closed issue's own status among its options the same
   *  way - so what the picker shows, what the toggle says and where the message goes stay one
   *  issue; a send to it is the server's to refuse (409 `ISSUE_CLOSED`). */
  const committedClosed =
    issueKey !== "" &&
    issues.data !== undefined &&
    !issues.data.some((issue) => issue.key === issueKey);
  const committedLabel = committedClosed ? `${issueKey} (closed)` : issueKey;
  // The picker exists to be used, so opening it hands over the control inside it - the same
  // move `MultiSelect` makes with its search box. It is what `i` needs (a key that opened
  // something no keystroke could then reach would be a dead end) and what a pointer wants too,
  // and it waits for the list rather than a frame, since the select renders only once the read
  // lands.
  //
  // That wait is the whole latency of `GET /issues`, and a reader who has roved on in the
  // meantime keeps where they went - `takeBack`'s rule above, widened to the row this composer
  // belongs to: focus is the picker's to take only while it is still where the open left it.
  // Once per open, so a refetch behind the reader never pulls them back either.
  const issueSelect = useRef<HTMLSelectElement>(null);
  const pickerTookFocus = useRef(false);
  useEffect(() => {
    if (!issuePickerOpen) return;
    setPendingIssue(issueKey);
  }, [issueKey, issuePickerOpen]);
  useEffect(() => {
    if (!issuePickerOpen) {
      pickerTookFocus.current = false;
      return;
    }
    // The list is the dependency that matters: the select renders only once it lands.
    if (issues.data === undefined || pickerTookFocus.current || issueSelect.current === null) {
      return;
    }
    // Where the open can have left focus, named: the row `i` was pressed on, the toggle a
    // pointer clicked, or nothing at all. A reader who has gone on - to another row, or into
    // this composer's own field - keeps where they went.
    const active = document.activeElement;
    const openedOn =
      focusOnDocument() ||
      active === box.current?.closest(AGENT_ROW_SELECTOR) ||
      (active instanceof Element && active.matches(ISSUE_PICKER_SELECTOR));
    if (!openedOn) return;
    pickerTookFocus.current = true;
    issueSelect.current.focus();
  }, [issuePickerOpen, issues.data]);
  // Replies retain their parent owner: issue-attached legacy messages stay on that issue's
  // message route, while issue-less roots keep the S3-deferred direct session channel.
  const replyIssueKey = replyTo?.issueKey;
  const useDirectChannel =
    replyIssueKey === null || (replyIssueKey === undefined && issueKey === "");
  const composerOwner = useDirectChannel
    ? { kind: "session" as const, sessionId: agent.session_id }
    : { issueKey: replyIssueKey ?? issueKey, kind: "issue" as const };

  return (
    <div className={`mt-3 border-t pt-3 ${borderDefault}`} ref={box}>
      {replyTo === null ? (
        <button
          aria-expanded={issuePickerOpen}
          aria-label="Choose issue"
          className={`min-h-11 rounded-lg border px-3 text-sm font-medium disabled:cursor-not-allowed ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
          data-agent-issue-picker=""
          disabled={sending}
          onClick={() => setIssuePickerOpen((open) => !open)}
          type="button"
        >
          Issue: {issueKey === "" ? "No issue" : committedLabel}
        </button>
      ) : null}
      {issuePickerOpen && replyTo === null ? (
        <div className="mt-2">
          {issues.isPending ? (
            <p className={`text-sm ${textMutedOnCanvas}`}>Loading issues…</p>
          ) : null}
          {issues.isError ? (
            <p className={`text-sm ${dangerText}`}>Could not load issues.</p>
          ) : null}
          {issues.data === undefined ? null : (
            <label className={`block text-sm font-medium ${textSecondaryOnCanvas}`}>
              Issue (optional)
              <select
                aria-label="Issue"
                className={`mt-1 block min-h-11 w-full rounded-lg px-3 py-2 text-sm font-normal disabled:cursor-not-allowed disabled:opacity-50 ${inputClasses(true)}`}
                data-agent-issue-select=""
                disabled={sending}
                onBlur={() => {
                  // A step is not a pick until `Enter`, so leaving the select any other way -
                  // Tab, Shift+Tab, a click elsewhere - drops it, as Escape does: the open select
                  // never shows an issue the message is not addressed to.
                  setPendingIssue(issueKey);
                }}
                onChange={(event) => {
                  setPendingIssue(event.target.value);
                  // A step from the select's own keys only moves the selection, so a keyboard
                  // reader can pass the first option to reach the second; `Enter` below is the
                  // pick. Any other change - a pointer's, or one made in the native popup - is a
                  // pick already made.
                  if (movedByKeyboard.current) return;
                  commitIssue(event.target.value);
                }}
                onKeyDown={(event) => {
                  if (event.key === "Enter") {
                    // The commit is this key's, and it stops here: left to bubble it would land
                    // in the message the pick just addressed, as a newline at its top.
                    event.preventDefault();
                    commitIssue(event.currentTarget.value);
                    return;
                  }
                  markKeyStep();
                }}
                onKeyPress={markKeyStep}
                ref={issueSelect}
                value={pendingIssue}
              >
                <option value="">No issue</option>
                {committedClosed ? (
                  <option disabled value={issueKey}>
                    {committedLabel}
                  </option>
                ) : null}
                {issues.data.map((issue) => (
                  <option key={issue.key} value={issue.key}>
                    {issue.key} · {issue.title}
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
      ) : null}
      <MentionComposer
        // The channel decides which mention the message needs - an issue comment reaches this
        // agent by mentioning it, a direct message does not. The composer takes a changed seed
        // into its live draft in place, so one instance holds the draft across every pick.
        seedMentions={
          useDirectChannel
            ? undefined
            : [{ target: `session:${agent.session_id}`, title: agent.title || agent.session_id }]
        }
        mutationKey={sendKey}
        onCancelReply={onCancelReply}
        onClose={() => {
          if (sentJustNow.current) {
            sentJustNow.current = false;
            return;
          }
          onClose();
        }}
        onSent={() => {
          sentJustNow.current = true;
          onCancelReply();
          void queryClient.invalidateQueries({
            queryKey: agentMessagesQuery(agent.session_id).queryKey,
          });
          // Focus went to the document when the field disabled itself; take it back.
          refocusComposer();
        }}
        owner={composerOwner}
        replyTo={replyTo?.target ?? null}
      />
    </div>
  );
}
