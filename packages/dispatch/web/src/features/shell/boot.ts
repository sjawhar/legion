import type { QueryClient } from "@tanstack/react-query";
import { whoAmIQuery } from "../../api/queries";
import { openDocumentEarly } from "../doc/open-document-early";
import { installChunkFailureRecovery } from "./DeploymentResilience";

/**
 * What the page does before it has rendered anything: install the chunk-failure recovery, ask
 * who is signed in, and open the address's own document.
 *
 * All three used to be module-scope calls in `main.tsx` (dispatch://INFRA-341, R11), which put
 * `api/queries` and the document's early read - and through them the live socket, the inbox's
 * freshness, the Markdown engine and the router's address rules - in the entry chunk, the one
 * the browser compiles and evaluates in a single main-thread task before anything of the
 * page's own runs. They belong to the page, not to its frame, so the entry asks for this
 * module instead and calls this the moment it arrives. Its chunk is downloaded beside the
 * entry's own from every address (`DOCUMENT_ADDRESS_MODULES` in `web/vite.config.ts`), so what
 * that costs is the task this module's graph evaluates in, not a request.
 *
 * The order is the order `main.tsx` kept, and both chunks are asked for in the same task, so
 * this module's graph - the smaller of the two - evaluates beside the app's rather than in
 * front of it, and whichever reaches a read first makes it. Who may read what is untouched:
 * the same reads, under the same keys, with the same session.
 */
export function bootThePage(client: QueryClient, pathname: string, search: string): void {
  // A chunk that fails to download while the page is online reloads it, from the tab's budget.
  // `web/index.html`'s inline script keeps the same rule for a script, stylesheet or preload
  // that failed before any of this bundle ran - the entry chunk, this module's own and the
  // app's among them - so the window before this call is covered where it matters.
  installChunkFailureRecovery();
  // The sign-in check. Every page waits on it - the shell renders its skeleton until it answers
  // (`app.tsx`'s `AuthGate`) - and it used to be asked for from that gate's own first commit,
  // about 800 ms into React's boot on a throttled phone.
  void client.prefetchQuery(whoAmIQuery());
  // The address this page was opened at, read before React renders anything: a document opens
  // on its own text, and nothing about that read waits on the shell, the sign-in check or the
  // issue page (`open-document-early.ts`). A later address opens from the shell's own effect,
  // and this one is not opened twice.
  openDocumentEarly(client, pathname, search);
}
