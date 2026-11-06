import type { ReactNode } from "react";

import { useTooltips } from "../../components/tooltips";

import {
  connectionDotConnected,
  connectionDotConnecting,
  connectionDotFailed,
  connectionDotOffline,
  textMutedOnSurface,
} from "../../theme/classes";
import { authorText } from "../issue/page-structure";
import { type ConnectionState, connectionLabel, pendingNotice } from "./connection";
import type { PendingState } from "./pending-sync";

const DOT_CLASS_BY_STATE: Record<ConnectionState, string> = {
  connected: connectionDotConnected,
  connecting: connectionDotConnecting,
  failed: connectionDotFailed,
  offline: connectionDotOffline,
};

/** The document's live-connection indicator. A pending-edit notice expands it only while the
 * reader needs one; otherwise it stays a compact dot in the document header. */
export function ConnectionDot({
  connection,
  pending,
}: {
  connection: ConnectionState;
  pending: PendingState | undefined;
}): ReactNode {
  const notice = pendingNotice(connection, pending);
  const label = connectionLabel(connection, notice);
  return (
    <span
      // The dot is named by `aria-label`, not by the `title` it used to lean on: a surface may
      // draw no tooltip at all (`components/tooltips.tsx`, the issue page) and the indicator
      // still has to say which state it is in.
      aria-label={label}
      className={
        notice === undefined
          ? "inline-flex shrink-0 items-center"
          : "inline-flex items-center gap-1"
      }
      role="status"
      title={useTooltips() ? label : undefined}
    >
      <span aria-hidden className={`h-2.5 w-2.5 rounded-full ${DOT_CLASS_BY_STATE[connection]}`} />
      {/* How the document's own socket stands, and how many edits are unsent: a reading of the
          document, not a word a page declares (`features/issue/page-structure.ts`). */}
      {notice === undefined ? null : (
        <span aria-hidden className={`text-xs ${textMutedOnSurface}`} {...authorText}>
          {notice}
        </span>
      )}
      <span className="sr-only" {...authorText}>
        {label}
      </span>
    </span>
  );
}
