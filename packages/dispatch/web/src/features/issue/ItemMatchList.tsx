import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { IDCandidate } from "../../api/types";
import {
  badgeLow,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { buildReferencePath, parseDispatchReference, shortForm } from "../refs/routes";
import type { ItemRoute } from "./useItemLanding";

/** What each kind of match is called, in the words the rest of the page uses for the same
 *  records. A `block` is a block of a document, an `artifact` the document itself. */
const candidateNoun: Record<IDCandidate["kind"], string> = {
  artifact: "Document",
  ask: "Ask",
  block: "Block",
  comment: "Comment",
  message: "Message",
};

/** Where the match stands: the issue that owns it, or the project document it is written on,
 *  read from that document's own reference (`CORE/design-notes`). A match with neither - a
 *  direct message - stands nowhere a reader can be sent, and names none. */
function ownerOf(candidate: IDCandidate): string | undefined {
  if (candidate.issue_key !== undefined && candidate.issue_key !== "") {
    return candidate.issue_key;
  }
  if (candidate.document === undefined || candidate.document === "") {
    return undefined;
  }
  const route = parseDispatchReference(candidate.document);
  return route === undefined ? candidate.document : shortForm(route);
}

/**
 * The matches an id written short names, as the server listed them in its refusal
 * (`ID_AMBIGUOUS`): each one's kind, where it stands, and a link whose address carries its whole
 * id, so the reader opens the one they meant and the address they then hold is unambiguous. It
 * is a view of the issue page at the address the reader followed, not a page of its own: there
 * is no address for a list of matches.
 *
 * A match the server gave no address for, a direct message a reader may not open, is listed as
 * its kind alone: nothing is linked to it, and no empty line is drawn where its first line would
 * have been.
 */
export function ItemMatchList({
  candidates,
  route,
}: {
  candidates: readonly IDCandidate[];
  route: ItemRoute;
}): ReactNode {
  return (
    <section>
      <h1 className={`mb-2 text-2xl font-semibold ${textPrimaryOnCanvas}`}>
        Several items match that link
      </h1>
      <p className={textSecondaryOnCanvas}>
        <code>{route.id}</code> names {candidates.length} items on {route.key}. Open the one you
        meant, or write more of the id.
      </p>
      <ul className="mt-4 space-y-3 text-sm">
        {candidates.map((candidate) => (
          <ItemMatch candidate={candidate} key={`${candidate.kind}:${candidate.id}`} />
        ))}
      </ul>
      <Link
        className={`mt-4 inline-block text-sm font-medium underline ${linkText} ${linkHoverText}`}
        to={`/issues/${route.key}`}
      >
        Back to {route.key}
      </Link>
    </section>
  );
}

function ItemMatch({ candidate }: { candidate: IDCandidate }): ReactNode {
  const reference = candidate.ref === "" ? undefined : parseDispatchReference(candidate.ref);
  const owner = ownerOf(candidate);
  // The first line tells two matches apart; where the server gave none, the whole id does, so
  // the link always has words of its own.
  const words = candidate.first_line ?? "";
  return (
    <li className="min-w-0">
      <div
        className={`flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5 text-xs ${textMutedOnCanvas}`}
      >
        <span className={`rounded-full px-2 py-0.5 font-medium ${badgeLow.bg} ${badgeLow.text}`}>
          {candidateNoun[candidate.kind]}
        </span>
        {owner === undefined ? null : (
          <span className="min-w-0 [overflow-wrap:anywhere]">{owner}</span>
        )}
      </div>
      {reference === undefined ? null : (
        <Link
          className={`block min-w-0 [overflow-wrap:anywhere] ${linkText} ${linkHoverText}`}
          to={buildReferencePath(reference)}
        >
          {words === "" ? candidate.id : words}
        </Link>
      )}
    </li>
  );
}
