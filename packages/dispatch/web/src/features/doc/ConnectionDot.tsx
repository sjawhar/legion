import type { ReactNode } from "react";

import {
  connectionDotConnected,
  connectionDotConnecting,
  connectionDotOffline,
} from "../../theme/classes";
import { type ConnectionState, connectionLabel } from "./connection";

const DOT_CLASS_BY_STATE: Record<ConnectionState, string> = {
  connected: connectionDotConnected,
  connecting: connectionDotConnecting,
  offline: connectionDotOffline,
};

/** The document's live-connection indicator: a small dot rather than a word, so it doesn't take
 * up header width. `role="status"` and the `sr-only` label keep it announced to screen readers
 * and readable by tests exactly as the old text label was. */
export function ConnectionDot({ connection }: { connection: ConnectionState }): ReactNode {
  const label = connectionLabel(connection);
  return (
    <span className="inline-flex shrink-0 items-center" role="status" title={label}>
      <span aria-hidden className={`h-2.5 w-2.5 rounded-full ${DOT_CLASS_BY_STATE[connection]}`} />
      <span className="sr-only">{label}</span>
    </span>
  );
}
