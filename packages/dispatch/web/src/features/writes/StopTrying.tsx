import type { ReactNode } from "react";

import { RefusableButton } from "../../components/RefusableButton";

/** Stop trying, for a late write - a composer's send, a broadcast, or any other write - busy while
 *  the reader has stopped it and its last attempt is still out (`stopping`). */
export function StopTrying({
  onStop,
  stopping,
}: {
  readonly onStop: () => void;
  readonly stopping: boolean;
}): ReactNode {
  return (
    <RefusableButton
      busy={stopping ? "Stopping…" : undefined}
      look="secondary-compact"
      onPress={onStop}
      refusal={null}
    >
      Stop trying
    </RefusableButton>
  );
}
