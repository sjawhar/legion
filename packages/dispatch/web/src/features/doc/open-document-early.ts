import type { QueryClient } from "@tanstack/react-query";

import { primarySpec } from "../../api/issue-cache";
import { issueQuery } from "../../api/queries";
import type { ArtifactText } from "../../api/types";
import { parseIssuePath } from "../refs/routes";
import { liveTextQueryOptions } from "./live-text";

/**
 * Starts the reads an open document needs, from the address alone, as early as the shell knows
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
 * What it does not change is who may read what. These are the page's own reads, made with the
 * page's own session under the same query keys, after the sign-in check has answered
 * (`AuthGate`); the page that mounts next finds each in the cache or in flight, and asks for it
 * itself exactly as it always did. A failure is left in the cache as any failed read is, for
 * that page to show and to retry.
 */
export function openDocumentEarly(client: QueryClient, pathname: string, search: string): void {
  // Whatever the address before this one left unread is this navigation's to drop, before
  // anything of this one is started: a read belongs to the opening it was made for.
  earlyTextRead = undefined;
  const route = parseIssuePath(pathname, search);
  // Every route whose page opens on a document: the Spec tab's, and a named artifact's, which is
  // a document or is nothing to read ahead for.
  if (route?.kind !== "issue" && route?.kind !== "spec" && route?.kind !== "artifact") {
    return;
  }
  void client
    .fetchQuery(issueQuery(route.key))
    .then((issue) => {
      const artifact =
        route.kind === "artifact"
          ? issue.artifacts.find(({ slug }) => slug === route.slug)
          : primarySpec(issue);
      // A blob artifact, and a slug this issue has no artifact for, open no document.
      if (artifact?.kind === "doc") {
        readTheTextEarly(client, artifact.id);
      }
    })
    .catch(() => undefined);
}

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

function readTheTextEarly(client: QueryClient, artifactId: string): void {
  // `staleTime: 0` because an admission takes this read: it has to be a read, never whatever
  // this key happened to hold.
  const read = client.fetchQuery({ ...liveTextQueryOptions(artifactId), staleTime: 0 });
  earlyTextRead = { artifactId, read };
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
