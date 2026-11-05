import type { QueryClient } from "@tanstack/react-query";

import { api } from "../../api/client";
import { primarySpec } from "../../api/issue-cache";
import { issueQuery, userStateQuery } from "../../api/queries";
import type { ArtifactText } from "../../api/types";
import { parseAheadOfTheRender, warmMarkdownRenderer } from "../refs/markdown-engine";
import { markdownWorkerThreshold } from "../refs/markdown-worker-threshold";
import { parseIssuePath } from "../refs/routes";
import { leadingBlocks } from "./document-blocks";
import { currentAddress, dropFirstScreen } from "./first-screen";
import { liveTextQueryOptions } from "./live-text";

/**
 * Starts the reads an open document needs, from the address alone, as early as the page knows
 * it: the issue, and then the document's own text.
 *
 * It exists because of what those reads used to wait behind. The document's first fetch began
 * about 3.5 s after the issue's own answer arrived on a phone, because nothing asked for it
 * until `IssuePage`'s chunk had downloaded, its two reads had answered, and React had rendered
 * the whole issue page down to `ProofDocument` - and that text is what the reader reads
 * (`DocumentPreview`), so every one of those was time a large document showed nothing
 * (dispatch://INFRA-341, R11). None of them is anything the text read depends on: the address
 * names the issue, the issue names the document, and the document names its text.
 *
 * The page's first address opens as the entry module evaluates (`main.tsx`), which is before
 * React has rendered anything and before the sign-in check has been asked for, let alone
 * answered: on a phone that moved the issue's read about a second earlier and the document's
 * text about two, since neither waits any longer for the shell to commit. Every later address
 * opens from the shell's own effect (`app.tsx`'s `OpenDocumentEarly`), which is when the page
 * learns of it. The two meet on the page's first render, so an address already opened is not
 * opened again: a second read of the same text under `staleTime: 0` would download the
 * document twice.
 *
 * What it does not change is who may read what. These are the page's own reads, made with the
 * page's own session under the same query keys, and Dispatch answers each exactly as it
 * answers the page's: a reader who may not read this issue is refused here as they are
 * refused everywhere, and the shell still shows them whatever the sign-in check says
 * (`AuthGate`). The page that mounts next finds each read in the cache or in flight, and asks
 * for it itself exactly as it always did. A failure is left in the cache as any failed read
 * is, for that page to show and to retry.
 */
export function openDocumentEarly(client: QueryClient, pathname: string, search: string): void {
  const address = `${pathname}${search}`;
  if (address === openedAddress) {
    return;
  }
  openedAddress = address;
  // The blocks the server drew into this page belong to the address it drew them for
  // (`first-screen.ts`). A navigation away from that address leaves them with nothing to adopt
  // them, so they go here, before anything of the new address is started.
  dropFirstScreen(currentAddress(pathname));
  // Whatever the address before this one left unread is this navigation's to drop, before
  // anything of this one is started: a read belongs to the opening it was made for.
  earlyTextRead = undefined;
  const route = parseIssuePath(pathname, search);
  // Every route whose page opens on a document: the Spec tab's, and a named artifact's, which is
  // a document or is nothing to read ahead for.
  if (route?.kind !== "issue" && route?.kind !== "spec" && route?.kind !== "artifact") {
    return;
  }
  // The text itself, where the address names the document: `/issues/{key}/artifacts/{slug}`
  // names it as Dispatch's own route does, so the bytes a reader reads are asked for in the
  // first round trip rather than the second, and `web/index.html` has usually asked for them
  // already - this call takes that request rather than making a second one
  // (`api/early-document-read.ts`). The issue read below still runs - the page needs it, and it
  // is what names the artifact this answer belongs to - but the document's 420 KB no longer
  // waits behind it, which on a phone was over a second of nothing on the screen
  // (dispatch://INFRA-341, R11). The Spec tab's address names no slug, so its text is read by
  // the id the issue answers with, as it always was.
  const bySlug =
    route.kind === "artifact" ? api.getIssueArtifactText(route.key, route.slug) : undefined;
  // Nothing waits on this promise until the issue read hands it over, so its refusal is
  // answered here rather than left to the window as an unhandled rejection.
  void bySlug?.catch(() => undefined);
  void client
    .fetchQuery(issueQuery(route.key))
    .then((issue) => {
      const artifact =
        route.kind === "artifact"
          ? issue.artifacts.find(({ slug }) => slug === route.slug)
          : primarySpec(issue);
      // A blob artifact, and a slug this issue has no artifact for, open no document.
      if (artifact?.kind === "doc") {
        readTheTextEarly(client, artifact.id, bySlug);
      }
    })
    // The issue could not be read, so nothing names a document to read ahead for.
    .catch(() => undefined);
  // The other read the page showing this document holds its whole render behind: the reader's
  // own issue state, which `IssuePage` waits for beside the issue itself (`useIssueDetail`) and
  // which the shell otherwise asks for only once it has rendered. The document draws inside
  // that page, so a read the page waits on is a read the document waits on.
  void client.prefetchQuery(userStateQuery());
  // The engine that draws the document, started beside those reads rather than behind them.
  // Its first screenful is parsed by the read-only headless engine (`DocumentPreview`), whose
  // schema read and 461 KB chunk the shell otherwise asks for only once it has rendered -
  // which on a phone put both, and the engine's own build, between the text arriving and the
  // first block being drawn from it (dispatch://INFRA-341, R11). It is started after the reads
  // above are out, since the build runs on the page's own thread in a long chain of
  // continuations and the reads' answers are read on that thread too. The warm-up is the
  // shell's own (`DeploymentResilience`), loads once however many callers ask, and draws
  // nothing itself.
  void warmMarkdownRenderer().catch(() => undefined);
}

/** The address this module last opened, so the entry's own call and the shell's first effect -
 *  which run for the same address, in that order - open it once between them. */
let openedAddress: string | undefined;

/**
 * The text read this module made for the document the address opened, waiting for the admission
 * that document's editor makes.
 *
 * An admission connects only on a read it knows was made for this opening of this document - a
 * browser editor normalizes a tree it cannot represent and writes the result back, so the
 * server's answer to a read from some earlier minute says nothing about the tree it would be
 * admitted to now (`ProofDocument`'s `admit`). This read is one of those: it was made because
 * this address was opened, and the document mounting is what it was made for. Handing it over
 * rather than letting the admission make its own is what keeps a large document from being
 * downloaded twice for one opening - once for the reader to read, once for the socket.
 *
 * So there is one, and it belongs to the navigation that made it: the next navigation drops
 * whatever is left of it, read or unread, before it starts its own. A reader who opens a
 * document from the issue's Artifacts tab, goes back, and opens it again has left a read of
 * that document behind them on the first visit - the Spec tab's, which nothing mounted to take
 * - and the tree can have changed since, as it does where the document was replaced by one
 * outside the Proof schema: an admission handed that read would open a socket the server then
 * refuses, where a read of its own tells it not to open one at all
 * (`e2e/artifacts.e2e.ts`, "an out-of-schema document does not reconnect from cached text").
 *
 * It is also taken once. A second admission - a socket the server refused, a repair, a re-mount
 * later - finds nothing here and reads afresh, which is what each of those needs.
 */
let earlyTextRead: { artifactId: string; read: Promise<ArtifactText> } | undefined;

/** The document's text, under the key the page reads it by. `asked` is the same read already
 *  in flight under the document's address, where the address named it: it is the same route's
 *  answer about the same document, made for this opening, so the cache takes it rather than
 *  asking a second time. One that fails says nothing about the document - the slug may name a
 *  file this issue no longer has - so the read by id is made in its place, as it would have
 *  been, and so is every retry. */
function readTheTextEarly(
  client: QueryClient,
  artifactId: string,
  asked?: Promise<ArtifactText>
): void {
  // `staleTime: 0` because an admission takes this read: it has to be a read, never whatever
  // this key happened to hold.
  const read = client.fetchQuery({
    ...liveTextQueryOptions(artifactId),
    queryFn:
      asked === undefined
        ? () => api.getArtifactText(artifactId)
        : () => asked.catch(() => api.getArtifactText(artifactId)),
    staleTime: 0,
  });
  earlyTextRead = { artifactId, read };
  // The screenful the reader meets, parsed in the task its text arrives in rather than in the
  // commit that mounts the issue page. `DocumentPreview` draws exactly this source under
  // exactly these policies, and it is under the worker's threshold, so without this the page
  // parses it on its own thread inside that commit - 294-545 ms of one task on a phone at 4x
  // CPU throttling, in a task already over the 500 ms a reader feels as a freeze
  // (dispatch://INFRA-341, R11). Nothing waits for it here: there is no page yet.
  void read
    .then(({ markdown }) => parseAheadOfTheRender(leadingBlocks(markdown, markdownWorkerThreshold)))
    .catch(() => undefined);
  void read.catch(() => {
    // A failed read is the document's own to report and to retry; what it must not do is wait
    // here for an admission to take.
    if (earlyTextRead?.read === read) {
      earlyTextRead = undefined;
    }
  });
}

/** The read above, for the admission that follows it, and never twice. */
export function takeEarlyTextRead(artifactId: string): Promise<ArtifactText> | undefined {
  if (earlyTextRead?.artifactId !== artifactId) {
    return undefined;
  }
  const { read } = earlyTextRead;
  earlyTextRead = undefined;
  return read;
}
