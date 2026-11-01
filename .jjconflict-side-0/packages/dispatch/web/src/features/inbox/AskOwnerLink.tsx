import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { InboxRow, OpenAskOwner } from "../../api/types";
import { TruncatedText } from "../../components/TruncatedText";
import { linkHoverText, linkText, textMutedOnCanvas } from "../../theme/classes";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import { askIssueKey } from "./ask-name";

/** The issue or project document an ask belongs to, as one truncating link: the issue's key and
 *  title, or the document's project and name. Its hover card is always the owner's. With `askId`
 *  the link opens that ask inside its owner (an Inbox row, whose own card is the ask); without it,
 *  the owner itself (an answers row, whose Open already goes to the ask). */
export function AskOwnerLink({
  askId,
  className = "",
  inboxOwner = false,
  owner,
}: {
  askId?: string;
  /** Flex sizing from the host's line, e.g. `grow`. */
  className?: string;
  /** Marks the link an Inbox row's `o` binding follows; a group header's link is never one. */
  inboxOwner?: boolean;
  owner: OpenAskOwner;
}): ReactNode {
  const marker = inboxOwner ? "" : undefined;
  if ("issue" in owner) {
    const { key, title } = owner.issue;
    return (
      <Link
        className={`flex min-w-0 items-baseline gap-2 text-sm ${linkText} ${linkHoverText} ${className}`}
        data-inbox-owner={marker}
        to={buildIssuePath(
          askId === undefined ? { key, kind: "issue" } : { id: askId, key, kind: "ask" }
        )}
        {...referenceTriggerProps({ key, kind: "issue" })}
      >
        <span className="shrink-0 font-semibold">{key}</span>
        <TruncatedText>{title}</TruncatedText>
      </Link>
    );
  }
  const { name, project, slug } = owner.document;
  return (
    <Link
      className={`min-w-0 truncate text-sm font-semibold ${linkText} ${linkHoverText} ${className}`}
      data-inbox-owner={marker}
      to={buildProjectPath({
        item: askId === undefined ? undefined : { id: askId, kind: "ask" },
        kind: "document",
        project,
        slug,
      })}
      {...referenceTriggerProps({ kind: "document", project, slug })}
    >
      <TruncatedText>
        {project} · {name}
      </TruncatedText>
    </Link>
  );
}

/** An Inbox row's owner, which opens the row's ask. A row read without its issue object names
 *  the issue by key; a row with neither an issue nor a document has nothing to link. */
export function InboxOwnerLink({ ask, rowOwner }: { ask: InboxRow; rowOwner: boolean }): ReactNode {
  const key = askIssueKey(ask);
  const owner: OpenAskOwner | null =
    ask.document !== undefined
      ? { document: ask.document }
      : key === null
        ? null
        : { issue: { key, title: ask.issue?.title ?? key } };
  if (owner === null) {
    return <p className={`truncate text-sm ${textMutedOnCanvas}`}>Document ask</p>;
  }
  return <AskOwnerLink askId={ask.id} className="grow" inboxOwner={rowOwner} owner={owner} />;
}
