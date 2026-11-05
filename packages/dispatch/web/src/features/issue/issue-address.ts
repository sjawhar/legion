import { itemFromSearch } from "@legion/contracts";

import { buildIssuePath, type IssueRoute } from "../refs/routes";
import type { TreeNode } from "./issue-tree";
import type { ConversationState, Preset } from "./page-words";

/**
 * What one address of the issue page says (dispatch://INFRA-1196 R19,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t9`).
 *
 * Every address the old page's eight tabs answered still resolves, and this module is the one
 * place that says what each one opens: which preset, which node the page unfolds the path to
 * and puts the focus on, which state the conversation is in, whether the node menu is open, and
 * which version of the preset's document is pinned. `refs/routes.ts` still parses the address;
 * this reads the route it answers.
 *
 * It is pure and holds no React, so the page, the margin's panel and the unit tests all read an
 * address the same way.
 */

/** The node an address names, before the tree is there to resolve it against. */
export type AddressTarget =
  /** One block, by the block id a `#b-` fragment or a route's block target holds. */
  | { readonly kind: "block"; readonly id: string }
  /** One ask, comment or message, by its own id. */
  | { readonly kind: "item"; readonly id: string }
  /** One node by its own id inside the issue, which is how a document is named by its slug. */
  | { readonly kind: "node"; readonly id: string };

/** What one address of the issue page opens. */
export interface IssueAddress {
  readonly conversation: ConversationState;
  /** What `?q=` narrows the preset to; `""` for an address that narrows nothing. */
  readonly filter: string;
  /** Whether the address opens the node menu, which `/artifacts` does (R16). */
  readonly nodeMenu: boolean;
  readonly preset: Preset;
  /** The node the address names, which the page unfolds the path to and focuses (R19). */
  readonly target: AddressTarget | undefined;
  /** The version of the preset's document the address pins (`?v=`). */
  readonly version: number | undefined;
}

/** Which preset each kind of route opens. Everything not named here is the Tree preset, which
 *  is the issue's own address. */
const ROUTE_PRESETS: Readonly<Record<string, Preset>> = {
  decisions: "decisions",
  plan: "plan",
  spec: "spec",
  verification: "checks",
};

/** The address of `preset` on `key`, which is the address its own tab carries (R19): the Checks
 *  preset keeps `/verification` and the Tree preset is the issue's own address. */
export function presetPath(key: string, preset: Preset): string {
  if (preset === "tree") return buildIssuePath({ key, kind: "issue" });
  if (preset === "checks") return buildIssuePath({ key, kind: "verification" });
  return buildIssuePath({ key, kind: preset });
}

/** The conversation's state each address names; every other address leaves the panel on what
 *  people said, which is the conversation the `/conversation` address always meant. */
const ROUTE_CONVERSATION_STATES: Readonly<Record<string, ConversationState>> = {
  activity: "everything",
  conversation: "words",
};

/** A positive integer `?v=` holds, or undefined for anything else. */
function pinnedVersion(search: string): number | undefined {
  const value = new URLSearchParams(search).get("v") ?? "";
  return /^[1-9]\d*$/.test(value) ? Number(value) : undefined;
}

/** The node `route` and `search` name, outside of a `#b-` fragment. */
function routeTarget(route: IssueRoute): AddressTarget | undefined {
  if (route.kind === "ask" || route.kind === "comment" || route.kind === "message") {
    return { id: route.id, kind: "item" };
  }
  if (route.kind === "artifact") {
    const block = "block" in route ? route.block : undefined;
    return block === undefined ? { id: route.slug, kind: "node" } : { id: block.id, kind: "block" };
  }
  if (route.kind === "spec") {
    const block = "block" in route ? route.block : undefined;
    return block === undefined ? undefined : { id: block.id, kind: "block" };
  }
  return undefined;
}

/**
 * What the address `route` + `search` + `hash` opens. `isPrimaryDocument` says the route names
 * the issue's own spec by its slug (`/artifacts/<spec>`), which is the Spec preset's address as
 * much as `/spec` is. The fragment outranks the route's own
 * target, because a reader who wrote `#b-` meant that block; `?ask=` and `?comment=` outrank
 * both, because they are what a deep link into a question or a comment carries.
 */
export function issueAddress(
  route: IssueRoute,
  search: string,
  hash: string,
  isPrimaryDocument: boolean
): IssueAddress {
  const query = new URLSearchParams(search);
  const item = itemFromSearch(search);
  const block = hash.startsWith("#b-") ? decodeURIComponent(hash.slice(3)) : undefined;
  const named: AddressTarget | undefined =
    block === undefined ? routeTarget(route) : { id: block, kind: "block" };
  return {
    conversation: ROUTE_CONVERSATION_STATES[route.kind] ?? "words",
    filter: query.get("q") ?? "",
    nodeMenu: route.kind === "artifacts",
    preset: isPrimaryDocument ? "spec" : (ROUTE_PRESETS[route.kind] ?? "tree"),
    target: item === null || item === undefined ? named : { id: item.id, kind: "item" },
    version: pinnedVersion(search) ?? (route.kind === "artifact" ? route.version : undefined),
  };
}

/** The chain from `root` down to the first node `wanted` answers for, the root first, or
 *  undefined when the tree holds no such node. */
function pathToNode(
  root: TreeNode,
  wanted: (node: TreeNode) => boolean
): readonly TreeNode[] | undefined {
  if (wanted(root)) return [root];
  for (const child of root.children) {
    const below = pathToNode(child, wanted);
    if (below !== undefined) return [root, ...below];
  }
  return undefined;
}

/**
 * The chain from the issue down to the node `target` names, the issue first. An ask is the
 * block the server indexed it from where it has one, so an item is looked up by its node id
 * first and by the `dispatch://` address of that item second.
 */
export function pathToTarget(
  root: TreeNode,
  target: AddressTarget | undefined
): readonly TreeNode[] | undefined {
  if (target === undefined) return undefined;
  if (target.kind === "block") {
    const suffix = `#b-${target.id}`;
    return pathToNode(root, (node) => node.id.endsWith(suffix));
  }
  const byId = pathToNode(root, (node) => node.id === target.id);
  if (byId !== undefined || target.kind === "node") return byId;
  const address = `/${target.id}`;
  return pathToNode(root, (node) => node.reference?.endsWith(address) === true);
}
