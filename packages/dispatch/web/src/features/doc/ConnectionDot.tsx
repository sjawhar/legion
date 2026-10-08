import type { ReactNode } from "react";

import {
  connectionDotConnected,
  connectionDotConnecting,
  connectionDotFailed,
  connectionDotOffline,
  textMutedOnSurface,
} from "../../theme/classes";
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
  const label = connectionLabel(connection, pending);
  const notice = pendingNotice(connection, pending);
  return (
    <span
      className={
        notice === undefined
          ? "inline-flex shrink-0 items-center"
          : "inline-flex items-center gap-1"
      }
      role="status"
      title={label}
    >
      <span aria-hidden className={`h-2.5 w-2.5 rounded-full ${DOT_CLASS_BY_STATE[connection]}`} />
      {notice === undefined ? null : (
        <span aria-hidden className={`text-xs ${textMutedOnSurface}`}>
          {notice}
        </span>
      )}
      <span className="sr-only">{label}</span>
    </span>
  );
}
