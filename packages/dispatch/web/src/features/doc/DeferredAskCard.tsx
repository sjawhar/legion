import { lazy, type ReactNode, Suspense } from "react";

import type { AskBlockHost } from "./typed-block-drawing";

/**
 * The decision card every Markdown body draws, fetched after the first paint rather than before
 * it.
 *
 * The shell provides one card for every body on the page (`TextAskCard` in
 * `features/refs/markdown-cards.ts`, provided in `app.tsx`), and providing it by a static import
 * put the whole Inbox ask card in the entry chunk: `AskCard`, `AskHeader`, `AskRecipients`,
 * `AskAnswerArea` and its composer, 49 modules and 75,165 bytes of the chunk the browser
 * downloads, compiles and evaluates in one main-thread task before anything of the page's own
 * runs (dispatch://INFRA-341, R11). No card is on the first frame of any route - a body has to
 * be read, parsed and painted before one of its `ask` shells exists - so none of it belongs in
 * that task.
 *
 * Loaded beside the card rather than in the shell, as the margin is (`features/margin/
 * MarginColumn.tsx`), because what the card draws is the card's own business. The shell provides
 * this component, so `useRenderedMarkdown` has a card from its first render and draws every
 * `ask` shell exactly as it does today; the card fills that shell in once its chunk arrives,
 * under the frame the body itself drew, which is the same shell a body with no provider leaves
 * empty. A document address downloads the chunk beside the issue page's own
 * (`documentAddressChunks` in `web/vite.config.ts`), so it is in hand before a body is parsed.
 */
const AskBlockCard = lazy(() =>
  import("./AskBlockCard").then((module) => ({ default: module.TextAskBlockCard }))
);

export function DeferredAskCard({ host }: { host: AskBlockHost }): ReactNode {
  return (
    <Suspense fallback={null}>
      <AskBlockCard host={host} />
    </Suspense>
  );
}
