import { api } from "../../api/client";
import type { Issue, NewIssueSettings } from "../../api/types";
import type { Writes } from "../writes/write-store";
import { press } from "./offer";

/**
 * The whole content of the Verification document a new issue from the bar gets: the words the
 * person typed, as one `check` block carrying no attributes. The words are a declaration of what
 * they want to be able to do, so they are the issue's first verification criterion word for
 * word; the block carries no attributes so Dispatch names no surface, no command and no control
 * of its own, and writes no word of its own into the issue. The spec and the plan are written
 * under it afterwards, by whoever takes the issue.
 */
export function firstCriterion(words: string): string {
  return `:::check{}\n${words}\n:::\n`;
}

/** What one acceptance of the offer writes. */
export interface NewIssueWrite {
  /** The project of the page the bar was opened on. */
  readonly project: string;
  /** The label the new issue carries and the session it goes to, as the deployment set them. */
  readonly settings: NewIssueSettings;
  /** The words the person typed, as they typed them. */
  readonly words: string;
}

/** What one acceptance made. */
export interface FiledIssue {
  readonly issue: Issue;
  /** The session the issue was routed to and its first message went to; `""` where the
   *  deployment names none, which is the issue made with no conversation started on it. */
  readonly session: string;
  /** The words it was made from, so a bar whose box has moved on drops the result. */
  readonly words: string;
}

/**
 * Files the person's typed words as an issue, through the routes every other client writes
 * through: the create, whose own related-item check answers whether anything already in the
 * project is a close match, then the Verification document holding the one criterion, and then,
 * where the deployment names a session, the issue's route and its first message to that session.
 *
 * Answers `undefined` for a create Dispatch made and had no answer to give for: the issue is
 * made, and nothing here knows which one, so the rest is the person's to write on it.
 */
export async function fileNewIssue(
  writes: Writes,
  { project, settings, words }: NewIssueWrite
): Promise<FiledIssue | undefined> {
  const issue = await press(writes, `New issue “${words}”`, (bounds) =>
    api.createIssue(
      { project, title: words, ...(settings.label === "" ? {} : { labels: [settings.label] }) },
      bounds
    )
  );
  if (issue === null) {
    return undefined;
  }
  await press(writes, `The first criterion of ${issue.key}`, (bounds) =>
    api.uploadArtifact(
      { issue: issue.key },
      { content: firstCriterion(words), name: "verification.md" },
      bounds
    )
  );
  if (settings.session !== "") {
    const target = `session:${settings.session}`;
    await press(writes, `${issue.key} to ${settings.session}`, (bounds) =>
      api.patchIssue(issue.key, { route: target }, bounds)
    );
    await press(writes, `The first message on ${issue.key}`, (bounds) =>
      api.createMessage(issue.key, { body: words, delivery: "aside", target }, bounds)
    );
  }
  return { issue, session: settings.session, words };
}
