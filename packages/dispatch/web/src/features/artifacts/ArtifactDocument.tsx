import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Navigate, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { Artifact, IDCandidate } from "../../api/types";
import type { Highlight } from "../doc/highlight";
import { type DocumentToolbar, ProofDocument } from "../doc/ProofDocument";
import { ItemMatchList } from "../issue/ItemMatchList";
import { ambiguousItems } from "../issue/useItemLanding";
import type { MarginOwner } from "../margin/useMarginItems";
import { referencesQuery, resolvedPrefixItems } from "../refs/reference-target";
import type { DispatchReferenceRoute } from "../refs/routes";
import { prefixReference, routeWithWholeIds } from "../refs/text-references";

/** The item a document address names, as `?comment=` or `?ask=` writes it. Its id may be written
 *  short, which only the reference it stands for can resolve. */
interface SelectedItem {
  id: string;
  kind: "ask" | "comment";
}

/** The reference the address names: the item under the owner of the document it was written on,
 *  which is the owner the server resolves a short id inside, as it does for the item routes. */
function itemReference(owner: MarginOwner, item: SelectedItem): DispatchReferenceRoute {
  return owner.kind === "issue"
    ? { id: item.id, key: owner.key, kind: item.kind }
    : { item, kind: "document", project: owner.project, slug: owner.slug };
}

/** The id `route` holds for an item of `kind`, once its short ids have been replaced; undefined
 *  for an answer that named a record of another kind. */
function itemId(route: DispatchReferenceRoute, kind: SelectedItem["kind"]): string | undefined {
  if (route.kind === "document") {
    return route.item?.kind === kind ? route.item.id : undefined;
  }
  return route.kind === kind ? route.id : undefined;
}

/** Where a document address's `?comment=` or `?ask=` lands, the query half of what
 *  `useItemLanding` does for `/issues/KEY/asks|comments|messages/<id>`. */
interface SelectedItemLanding {
  /** The matches an ambiguous short id named, for the list the item routes show; undefined for
   *  every other answer. */
  candidates: readonly IDCandidate[] | undefined;
  /** The item's whole id, the only id it is ever read under: undefined while a short id is
   *  being resolved and for one that names nothing here, which reads as an unknown id does. */
  id: string | undefined;
  /** The address with the whole id in place of the short one, or undefined where the address
   *  already holds it and there is nothing to replace. */
  rewrite: string | undefined;
}

/**
 * Resolves the id the address writes for `item`, then says what the page should do with it: a
 * whole id is kept as written and read under itself, with no added request; a short one is
 * resolved through the reference it stands for (`GET /references?to=`), as every other surface
 * resolves one, and the address is then replaced with the whole id so what a reader copies off
 * the page is unambiguous.
 */
function useSelectedItem(owner: MarginOwner, item: SelectedItem | undefined): SelectedItemLanding {
  const origin = window.location.origin;
  const { hash, pathname, search } = useLocation();
  const route = item === undefined ? undefined : itemReference(owner, item);
  // Only an id written short is resolved; `prefixReference` gives none for a whole one.
  const reference = route === undefined ? undefined : prefixReference(route);
  const resolution = useQuery({ ...referencesQuery(reference), enabled: reference !== undefined });
  const answered =
    route === undefined || reference === undefined
      ? route
      : routeWithWholeIds(route, resolvedPrefixItems(resolution.data), origin);
  const whole =
    item === undefined || answered === undefined ? undefined : itemId(answered, item.kind);
  const candidates = reference === undefined ? undefined : ambiguousItems(resolution.error);
  if (item === undefined || whole === undefined || whole === item.id) {
    return { candidates, id: whole, rewrite: undefined };
  }
  const params = new URLSearchParams(search);
  params.set(item.kind, whole);
  return { candidates, id: whole, rewrite: `${pathname}?${params.toString()}${hash}` };
}

export function ArtifactDocument({
  artifact,
  askId,
  commentId,
  highlightTerm,
  isClosed,
  owner,
  onToolbarChange,
  onVersionChange,
  showDiff,
  version,
}: {
  artifact: Artifact;
  askId?: string;
  commentId?: string;
  highlightTerm?: string;
  isClosed: boolean;
  owner: MarginOwner;
  showDiff: boolean;
  onToolbarChange?(toolbar: DocumentToolbar | undefined): void;
  onVersionChange: (version: number | null) => void;
  version: number | undefined;
}): ReactNode {
  const user = useQuery(whoAmIQuery());
  const selected: SelectedItem | undefined =
    commentId === undefined
      ? askId === undefined
        ? undefined
        : { id: askId, kind: "ask" }
      : { id: commentId, kind: "comment" };
  const landing = useSelectedItem(owner, selected);
  // The ask is read under its whole id alone: a prefix is no key anything is cached under. The
  // comment list is the document's own, the same read whichever comment the address names.
  const askRead = selected?.kind === "ask" ? landing.id : undefined;
  const comments = useQuery({
    enabled: commentId !== undefined,
    queryKey:
      owner.kind === "issue"
        ? ["comments", owner.key, artifact.id]
        : ["artifact", owner.artifactId, "comments"],
    queryFn: () =>
      owner.kind === "issue"
        ? api.listComments(owner.key, artifact.id)
        : api.listArtifactComments(owner.artifactId),
  });
  const ask = useQuery({
    enabled: askRead !== undefined,
    queryKey: ["ask", askRead],
    queryFn: () => {
      if (askRead === undefined) {
        throw new Error("Selected ask query requires an ask id.");
      }
      return api.getAsk(askRead);
    },
  });
  const selectedItem =
    selected === undefined
      ? undefined
      : selected.kind === "ask"
        ? ask.data?.ask
        : comments.data?.find((item) => item.id === landing.id);
  const anchor = selectedItem?.anchor;
  const highlight: Highlight | undefined =
    selectedItem === undefined ||
    anchor === null ||
    anchor === undefined ||
    (anchor.orphaned && version === undefined) ||
    anchor.artifact_id !== artifact.id
      ? undefined
      : {
          by: `${selectedItem.author.kind}:${selectedItem.author.id}`,
          id: selectedItem.id,
          quote: anchor.quote,
        };

  if (user.data === undefined) {
    return <p>Loading document…</p>;
  }

  // An ambiguous short id is the one answer that replaces the document: the reader picks which
  // item they meant, from the list the item routes show, at the address they followed.
  if (landing.candidates !== undefined && selected !== undefined && owner.kind === "issue") {
    return (
      <ItemMatchList
        candidates={landing.candidates}
        route={{ id: selected.id, key: owner.key, kind: selected.kind }}
      />
    );
  }

  return (
    <>
      {/* The replaced address adds no history entry, so Back still leaves the page the reader
          came from; the document stays mounted through it. */}
      {landing.rewrite === undefined ? null : <Navigate replace to={landing.rewrite} />}
      <ProofDocument
        artifact={artifact}
        highlight={highlight}
        highlightTerm={highlightTerm}
        isClosed={isClosed}
        onToolbarChange={onToolbarChange}
        owner={owner}
        showDiff={showDiff}
        key={artifact.id}
        onVersionChange={onVersionChange}
        user={user.data}
        version={version}
      />
    </>
  );
}
