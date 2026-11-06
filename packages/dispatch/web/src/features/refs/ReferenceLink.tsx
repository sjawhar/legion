import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { linkHoverText, linkText } from "../../theme/classes";
import { referenceTriggerProps } from "./RefPreview";
import { buildReferencePath, parseNodeReference, shortForm } from "./routes";

/**
 * A `dispatch://` reference drawn as a link to what it names, with the preview the whole app
 * shows when a reader hovers it. The words are `label` when the caller knows a better name — a
 * block's own words, read from the reference graph — and the reference's short form otherwise.
 * A reference this app can route nowhere is drawn as the text it is, since there is nowhere to
 * go.
 */
export function ReferenceLink({
  label,
  reference,
  testId,
}: {
  label?: string;
  reference: string;
  testId?: string;
}): ReactNode {
  const route = parseNodeReference(reference);
  if (route === undefined) {
    return <span data-testid={testId}>{reference}</span>;
  }
  return (
    <Link
      {...referenceTriggerProps(route)}
      className={`underline ${linkText} ${linkHoverText}`}
      data-testid={testId}
      to={buildReferencePath(route)}
    >
      {label ?? shortForm(route)}
    </Link>
  );
}
