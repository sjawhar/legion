import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { linkHoverText, linkText, textSecondaryOnCanvas } from "../../theme/classes";
import { useDocumentTitle } from "./useDocumentTitle";

/** Shared not-found view for both an unrecognized top-level route and an unrecognized issue sub-route. */
export function NotFoundPage(): ReactNode {
  useDocumentTitle("Not found · Dispatch");
  return (
    <section>
      <h1 className="mb-2 text-2xl font-semibold">Page not found</h1>
      <p className={textSecondaryOnCanvas}>That link doesn&apos;t match a page in Dispatch.</p>
      <Link
        className={`mt-4 inline-block text-sm font-medium underline ${linkText} ${linkHoverText}`}
        to="/"
      >
        Back to inbox
      </Link>
    </section>
  );
}
