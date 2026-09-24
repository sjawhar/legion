import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { primaryButtonBg, primaryButtonHoverBg, textSecondaryOnCanvas } from "../../theme/classes";
import { useDocumentTitle } from "./useDocumentTitle";

/** Shared not-found view: an unrecognized top-level route, an unrecognized issue sub-route, and
 *  an issue item whose id names nothing, which passes its own `title` and `detail`. */
export function NotFoundPage({
  backTo = "/",
  backLabel = "Back to inbox",
  detail = "That link doesn't match a page in Dispatch.",
  title = "Page not found",
}: {
  backLabel?: string;
  backTo?: string;
  detail?: string;
  title?: string;
} = {}): ReactNode {
  useDocumentTitle("Not found · Dispatch");
  return (
    <section>
      <h1 className="mb-2 text-2xl font-semibold">{title}</h1>
      <p className={textSecondaryOnCanvas}>{detail}</p>
      <Link
        className={`mt-4 inline-block rounded-lg px-3 py-2 text-sm font-medium ${primaryButtonBg} ${primaryButtonHoverBg}`}
        to={backTo}
      >
        {backLabel}
      </Link>
    </section>
  );
}
