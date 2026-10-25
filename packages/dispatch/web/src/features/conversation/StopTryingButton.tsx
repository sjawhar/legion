import type { ReactNode } from "react";

import { RefusableButton } from "../../components/RefusableButton";
import { type HeldSend, stoppable } from "./held-sends";

/** Stop trying, for a late send to one session the reader can stop (`stoppable`) or has stopped
 *  while its last attempt is still out; nothing for any other send. */
export function StopTryingButton({
  held,
  onStop,
}: {
  readonly held: HeldSend | undefined;
  readonly onStop: () => void;
}): ReactNode {
  if (held === undefined || !(stoppable(held) || held.stopping)) return null;
  return (
    <RefusableButton
      busy={held.stopping ? "Stopping…" : undefined}
      look="secondary-compact"
      onPress={onStop}
      refusal={null}
    >
      Stop trying
    </RefusableButton>
  );
}
