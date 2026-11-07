import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useState } from "react";

import { newIssueSettingsQuery } from "../../api/queries";
import { useWrites, type Writes } from "../writes/write-store";
import { type FiledIssue, fileNewIssue, type NewIssueWrite } from "./new-issue";
import { barNoteClassName, refusalText } from "./offer";

/** The group the offer's one row sits in, and the row's own id, which the bar's arrows and
 *  `aria-activedescendant` read. */
export const NEW_ISSUE_GROUP_ID = "search-group-new-issue";
export const NEW_ISSUE_OPTION_ID = "search-option-new-issue";

/** Where the offer stands: nothing pressed, the writes out, the issue made (`filed` absent for
 *  one Dispatch had no answer naming), or a refusal in the person's words. */
type OfferState =
  | { readonly kind: "idle" }
  | { readonly kind: "making"; readonly words: string }
  | { readonly filed: FiledIssue | undefined; readonly kind: "made"; readonly words: string }
  | { readonly kind: "refused"; readonly refusal: string; readonly words: string };

/** What the offer is at this moment: the row the bar lists for it, what pressing that row does,
 *  and the line the bar says underneath. */
export interface NewIssueOffer {
  /** The row's words, which say what pressing it does. */
  readonly label: string;
  /** Press it: accept the offer, or, once the issue is made, open it. */
  readonly select: () => void;
  /** What the bar says under the list about the issue it made or could not make. */
  readonly said: ReactNode;
}

/** How the offer writes; the tests pass their own. */
export type FileNewIssue = (
  writes: Writes,
  write: NewIssueWrite
) => Promise<FiledIssue | undefined>;

/**
 * The bar's offer of the words a person typed as a new issue, for words its search matched
 * nothing with. Offering is not making: nothing is written until the person presses the row.
 *
 * The press writes the issue (`fileNewIssue`) and then: with a session configured, opens the
 * issue, since a conversation is running on it; with none, says so and leaves the row as the way
 * to open it, because an issue nobody was asked about is one the person decides what to do with.
 */
export function useNewIssueOffer({
  file = fileNewIssue,
  offered,
  onOpen,
  project,
  words,
}: {
  /** Write seam for tests; defaults to the real sequence. */
  readonly file?: FileNewIssue;
  /** Whether the words matched nothing, so there is an offer to make at all. */
  readonly offered: boolean;
  /** Opens the issue the press made. */
  readonly onOpen: (key: string) => void;
  /** The project of the page the bar was opened on. */
  readonly project: string | undefined;
  /** The words in the box, as the person typed them. */
  readonly words: string;
}): NewIssueOffer | undefined {
  const queryClient = useQueryClient();
  const writes = useWrites();
  // The two settings decide what the press writes, so the row waits for them: a press made
  // without them would file an issue carrying neither the label nor the session the deployment
  // asked for. A read that failed leaves the bar offering nothing, as it does with no project.
  const settings = useQuery({
    ...newIssueSettingsQuery(),
    enabled: offered && project !== undefined,
  });
  const [state, setState] = useState<OfferState>({ kind: "idle" });
  // A box the person has typed on since is another question, so what the words before it made
  // is no longer this offer's answer.
  if (state.kind !== "idle" && state.words !== words) {
    setState({ kind: "idle" });
  }
  if (!offered || project === undefined || settings.data === undefined) {
    return undefined;
  }
  const here: OfferState =
    state.kind !== "idle" && state.words === words ? state : { kind: "idle" };
  const filed = here.kind === "made" ? here.filed : undefined;
  const accept = () => {
    const settled = settings.data;
    setState({ kind: "making", words });
    file(writes, { project, settings: settled, words }).then(
      (made) => {
        void queryClient.invalidateQueries({ queryKey: ["issues"] });
        setState({ filed: made, kind: "made", words });
        if (made !== undefined && made.session !== "") {
          onOpen(made.issue.key);
        }
      },
      (error: unknown) => {
        setState({
          kind: "refused",
          refusal: refusalText(error, "the write did not reach Dispatch."),
          words,
        });
      }
    );
  };
  return {
    label: filed === undefined ? `Make a new issue from “${words}”` : `Open ${filed.issue.key}`,
    said: <NewIssueSaid state={here} />,
    select: () => {
      if (filed !== undefined) {
        onOpen(filed.issue.key);
        return;
      }
      if (here.kind !== "making") {
        accept();
      }
    },
  };
}

/** The one line the bar says about the issue it is making, made, or could not make. */
function NewIssueSaid({ state }: { readonly state: OfferState }): ReactNode {
  if (state.kind === "idle") {
    return null;
  }
  if (state.kind === "making") {
    return (
      <p className={barNoteClassName} role="status">
        Making the issue…
      </p>
    );
  }
  if (state.kind === "refused") {
    return (
      <p className={barNoteClassName} data-testid="search-new-issue-said" role="alert">
        Could not make the issue: {state.refusal}
      </p>
    );
  }
  return (
    <p className={barNoteClassName} data-testid="search-new-issue-said" role="status">
      {state.filed === undefined
        ? "The issue is made, and Dispatch had no answer naming it."
        : state.filed.session === ""
          ? `${state.filed.issue.key} is made. No conversation was started: this Dispatch names no session for a new issue.`
          : `${state.filed.issue.key} is made, and its first message went to the session ${state.filed.session}.`}
    </p>
  );
}
