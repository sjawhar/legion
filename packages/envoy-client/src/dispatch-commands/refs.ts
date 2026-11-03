import { decodeBlockId, encodeBlockId, itemFromSearch } from "@legion/contracts";
import { commandLine, commandName } from "../dispatch-command";
import {
  dispatchChildRef,
  dispatchDocumentRef,
  dispatchIssueRef,
  type ItemKind,
  type ParsedDispatchRef,
  parseDispatchRef,
} from "../dispatch-owner";
import { ToolInputError } from "../tool-input-errors";
import { optionalString, stringArg, type ToolArguments } from "./context";

const canonicalUUIDPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const compactUUIDPattern = /^[0-9a-f]{32}$/i;

/** Canonicalizes every UUID spelling accepted by github.com/google/uuid.Parse. */
export function normalizeUUID(value: string): string | undefined {
  let compact = value;
  if (value.slice(0, 9).toLowerCase() === "urn:uuid:") {
    compact = value.slice(9);
  } else if (value.length === 38 && value.startsWith("{") && value.endsWith("}")) {
    compact = value.slice(1, -1);
  }
  if (canonicalUUIDPattern.test(compact)) compact = compact.replaceAll("-", "");
  if (!compactUUIDPattern.test(compact)) return undefined;
  return `${compact.slice(0, 8)}-${compact.slice(8, 12)}-${compact.slice(12, 16)}-${compact.slice(16, 20)}-${compact.slice(20)}`.toLowerCase();
}

export const askIdProblem = "ask must be a bare ask id or a dispatch://.../ask/<id> reference";
export const commentIdProblem =
  "comment must be a bare comment id or a dispatch://.../comment/<id> reference";
export const messageIdProblem =
  "in_reply_to must be a full message id (uuid), a dispatch://KEY/message/<id> reference, or a block of one (dispatch://KEY/message/<id>#b-<block id>)";

/** The ask id a bare id or dispatch://.../ask/<id> names; `argumentProblems` refused anything else. */
export function askId(args: ToolArguments): string {
  const ask = stringArg(args, "ask");
  return ask.startsWith("dispatch://") ? (parseDispatchRef(ask)?.id ?? ask) : ask;
}

/** The message uuid a bare id or dispatch://KEY/message/<id> names, or undefined for anything else. */
export function messageIdOf(value: string): string | undefined {
  const id = value.startsWith("dispatch://")
    ? (() => {
        const reference = parseDispatchRef(value);
        return reference?.kind === "message" ? reference.id : undefined;
      })()
    : value;
  return id === undefined ? undefined : normalizeUUID(id);
}

/** A block reference to a part of an ask, or to a block of a comment or message: the item and
 *  the block's id. */
type ItemBlockRef = ParsedDispatchRef & {
  readonly item: NonNullable<ParsedDispatchRef["item"]>;
  readonly block: string;
};

/** The part of an ask, or the block of a comment or message, a block reference names
 *  (`dispatch://KEY/ask/<id>#b-<part id>`, `dispatch://KEY/comment/<id>#b-<block id>`), or
 *  undefined for any other value: the block `reply_to` or `in_reply_to` answers, and the part
 *  `reply_to_ask` answers under its ask. */
function itemBlockOf(value: string): ItemBlockRef | undefined {
  const ref = parseDispatchRef(value);
  return ref?.item === undefined || ref.block === undefined
    ? undefined
    : { ...ref, item: ref.item, block: ref.block };
}
/** The address of the ask or comment `id` under a parsed ref's owner (a project-owned ref names its document). */
export function refTarget(ref: ParsedDispatchRef, kind: "ask" | "comment", id: string): string {
  const ownerRef =
    ref.owner.kind === "issue"
      ? dispatchIssueRef(ref.owner.issue)
      : dispatchDocumentRef(ref.owner.project, `${ref.artifact}`);
  return dispatchChildRef(ownerRef, kind, id);
}
/** The indefinite article a kind of item takes, with the kind: "an ask", "a comment". */
function article(kind: string): string {
  return kind === "ask" ? "an ask" : `a ${kind}`;
}

/**
 * Why `value`, given to `field` of a `kind` reply, names no block that reply can answer: a block
 * of another kind of item (a document's included), or one pinned to a revision, since a reply
 * answers the block whatever its text becomes. A part of an ask is what `reply_to_ask` answers,
 * as a comment's block is what `reply_to` answers. Undefined for a block of a `kind` and for a
 * value that is no block reference at all.
 */
export function replyBlockProblem(
  field: "reply_to" | "in_reply_to" | "reply_to_ask",
  kind: ItemKind,
  value: string
): string | undefined {
  if (!/^dispatch:\/\/[^#]*#b-/.test(value)) return undefined;
  const form = `dispatch://KEY/${kind}/<id>#b-<block id>`;
  const ref = parseDispatchRef(value);
  if (ref?.kind !== "block") {
    return `${field} ${value} names no block; a block of ${article(kind)} reads ${form}`;
  }
  const other = ref.item?.kind;
  if (other !== kind) {
    const answer =
      other === "comment"
        ? `; answer it with ${commandLine("dispatch_comment", { reply_to: value })}`
        : other === "message"
          ? `; answer it with ${commandLine("dispatch_message", { in_reply_to: value })}`
          : other === "ask"
            ? `; answer it with ${commandLine("dispatch_comment", { reply_to_ask: value })}`
            : "";
    const named = other === undefined ? "a document" : article(other);
    // `reply_to_ask` answers one part of the ask it names - its question, an option or its
    // answer - not a block another item holds.
    const answers =
      kind === "ask"
        ? "reply_to_ask answers a part of the ask it names"
        : `${article(kind)} reply answers a block of ${article(kind)}`;
    return `${field} ${value} names a block of ${named}, but ${answers} (${form})${answer}`;
  }
  if (ref.pin !== undefined) {
    return `${field} ${value} names revision ${ref.pin} of the block, but a reply answers the block whatever its text becomes: drop @v${ref.pin}`;
  }
  return undefined;
}
/** A short id: 8+ hex characters (hyphens allowed) that is not a full uuid. */
export const idPrefixPattern = /^[0-9a-f][0-9a-f-]{7,}$/i;

/** How a refusal names the owner whose asks or comments a prefix was matched against. */
export function refOwnerName(ref: ParsedDispatchRef): string {
  return ref.owner.kind === "issue"
    ? ref.owner.issue
    : `${ref.owner.project}/${ref.artifact ?? ref.id}`;
}

/** The owner fields of an ask or comment a reference is checked against. Each optional field
 *  admits `undefined` as well as being absent: under `exactOptionalPropertyTypes` the two are
 *  not the same, and an `Ask` or a `Comment` read off its zod schema carries the first. */
interface RefItem {
  readonly id: string;
  readonly issue_key: string | null;
  readonly artifact_id?: string | null | undefined;
  readonly anchor?: { readonly artifact_id: string } | null | undefined;
  readonly block_artifact?: { readonly id: string } | undefined;
  readonly approval?: { readonly artifact_id: string } | undefined;
}

/**
 * Refuses an ask or comment a reference names under an owner it is not on. A full id is read as
 * it is, whatever owner the reference names, so the item read is held to that owner: its issue,
 * or for a project document's reference, that document (`documentId`, read only then).
 */
export async function refuseItemOffRef(
  tool: string,
  kind: "ask" | "comment",
  ref: ParsedDispatchRef,
  refText: string,
  item: RefItem,
  documentId: () => Promise<string>
): Promise<void> {
  if (ref.owner.kind === "issue") {
    if (item.issue_key === ref.owner.issue) return;
  } else if (item.issue_key === null) {
    const id = await documentId();
    if (
      [
        item.artifact_id,
        item.anchor?.artifact_id,
        item.block_artifact?.id,
        item.approval?.artifact_id,
      ].includes(id)
    ) {
      return;
    }
  }
  const where =
    item.issue_key === null
      ? "another document"
      : `${item.issue_key}; name it dispatch://${item.issue_key}/${kind}/${item.id}`;
  throw new ToolInputError(tool, [
    `${refText} names ${kind} ${item.id} on ${refOwnerName(ref)}, but it is on ${where}`,
  ]);
}

/**
 * The full id an ask or comment reference names. A full uuid passes through; a unique prefix
 * is resolved against `list()`, the candidate set `ownerName` names; anything else is refused.
 */
export async function resolveIdPrefix(
  tool: string,
  kind: "ask" | "comment",
  id: string,
  ownerName: string,
  list: () => Promise<readonly { readonly id: string }[]>
): Promise<string> {
  const fullID = normalizeUUID(id);
  if (fullID !== undefined) return fullID;
  if (!idPrefixPattern.test(id)) {
    throw new ToolInputError(tool, [
      `${kind} id ${id} must be a full uuid or a prefix of at least 8 hex characters`,
    ]);
  }
  const prefix = id.toLowerCase();
  const matches = (await list()).filter((item) => item.id.toLowerCase().startsWith(prefix));
  if (matches.length === 1 && matches[0] !== undefined) return matches[0].id;
  throw new ToolInputError(tool, [
    matches.length === 0
      ? `${kind} id ${id} matches none of the ${kind}s on ${ownerName}; use the full id`
      : `${kind} id ${id} matches ${matches.length} ${kind}s on ${ownerName}; use the full id`,
  ]);
}
/** A reply's target: the ask, comment or message it answers and the part or block of that item
 *  it names, if any - a part of the ask a reply threads under (`reply_to_ask`), or a block of
 *  the comment or message it answers. */
export interface ReplyTarget {
  readonly id: string;
  readonly block?: string;
}

/** The validated reply target; `argumentProblems` refused anything that is not a message id, a
 *  message reference, or a block of a message. */
export function messageInReplyTo(args: ToolArguments): ReplyTarget | undefined {
  const inReplyTo = optionalString(args, "in_reply_to");
  if (inReplyTo === undefined) return undefined;
  const block = itemBlockOf(inReplyTo);
  if (block !== undefined && block.item.kind !== "message") return undefined;
  const id = messageIdOf(block?.item.id ?? inReplyTo);
  return id === undefined
    ? undefined
    : { id, ...(block === undefined ? {} : { block: block.block }) };
}

/** The item a `reply_to` or a `reply_to_ask` names: a comment's block reference splits into the
 *  comment's id and the block's, and a part reference into the ask's id and the part's;
 *  `argumentProblems` refused any other block reference, and Dispatch checks any other value as
 *  the item's id. */
export function itemReplyTo(
  args: ToolArguments,
  field: "reply_to" | "reply_to_ask"
): ReplyTarget | undefined {
  const replyTo = optionalString(args, field);
  if (replyTo === undefined) return undefined;
  const block = itemBlockOf(replyTo);
  return block === undefined ? { id: replyTo } : { id: block.item.id, block: block.block };
}
/**
 * The dispatch:// form of a dashboard URL on the configured server, or undefined when the
 * value is not such a URL. Accepts the issue, spec, artifact, ask, comment, log and activity
 * pages, the Decisions page (which names the issue's decisions.md), project document pages (with
 * their ?ask= / ?comment= deep links, which name the item whatever the fragment), the
 * `#b-<block id>` of a live document page or of an issue ask's, comment's or message's page,
 * which names that block, and the page of an upload an agent's conversation owns
 * (`/agents/<session id>/artifacts/<slug>`).
 */
export function dispatchRefFromUrl(value: string, serverUrl: string): string | undefined {
  let url: URL;
  let origin: string;
  try {
    url = new URL(value);
    origin = new URL(serverUrl).origin;
  } catch {
    return undefined;
  }
  if (url.origin !== origin) return undefined;
  // The SPA's version selector: `?v=N` on issue pages, `?version=N` on project document pages.
  const versionOf = (name: string): string => {
    const value = url.searchParams.get(name);
    return value !== null && /^[1-9][0-9]*$/.test(value) ? `@v${value}` : "";
  };
  // The block a page lands on (`#b-<block id>`), written as `encodeBlockId` writes it, a pin being
  // written only as a dispatch:// reference. A document's version page (`versionName` set) names
  // the document whatever its fragment: a version's stored markdown carries no block ids to land
  // on.
  const blockOf = (versionName?: string): string => {
    const id = url.hash.startsWith("#b-") ? decodeBlockId(url.hash.slice(3)) : undefined;
    return id === undefined || (versionName !== undefined && url.searchParams.get(versionName))
      ? ""
      : `#b-${encodeBlockId(id)}`;
  };
  const issuePage = url.pathname.match(
    /^\/issues\/([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?:\/(spec|log|conversation|activity|children|decisions)|\/artifacts\/([^/]+)|\/asks\/([^/]+)|\/comments\/([^/]+)|\/messages\/([^/]+))?\/?$/
  );
  if (issuePage) {
    const [, key = "", page, artifact, ask, comment, message] = issuePage;
    const version = versionOf("v");
    // A document page carrying `?comment=`/`?ask=` names that item, not the document, whatever
    // its fragment (the dashboard writes a document block's fragment beside `?comment=`): that is
    // the href `dispatch_search` returns for an anchored comment or ask on an issue's document.
    if (page === "spec" || artifact !== undefined) {
      const item = itemFromSearch(url.search);
      if (item === null) return undefined;
      if (item !== undefined) return `dispatch://${key}/${item.kind}/${item.id}`;
    }
    if (page === "spec" && version !== "") return `dispatch://${key}/artifact/spec${version}`;
    if (page === "spec") return `dispatch://${key}/spec${blockOf("v")}`;
    // The Decisions tab is a view of the issue's decisions.md, so its page names that document,
    // whatever its fragment.
    if (page === "decisions") return `dispatch://${key}/artifact/decisions-md`;
    // `/conversation` keeps resolving to the reference it always had, `KEY/log`; `/activity`,
    // the tab that now holds the event list, is spelled as its own path is.
    if (page !== undefined) return `dispatch://${key}/${page === "conversation" ? "log" : page}`;
    if (artifact !== undefined) {
      const slug = decodeURIComponent(artifact);
      return `dispatch://${key}/artifact/${slug}${version}${blockOf("v")}`;
    }
    if (ask !== undefined) return `dispatch://${key}/ask/${ask}${blockOf()}`;
    if (comment !== undefined) return `dispatch://${key}/comment/${comment}${blockOf()}`;
    if (message !== undefined) return `dispatch://${key}/message/${message}${blockOf()}`;
    return `dispatch://${key}`;
  }
  const agentArtifactPage = url.pathname.match(/^\/agents\/([^/]+)\/artifacts\/([^/]+)\/?$/);
  if (agentArtifactPage) {
    const [, session, slug] = agentArtifactPage;
    try {
      return `dispatch://agent/${decodeURIComponent(session ?? "")}/artifact/${decodeURIComponent(slug ?? "")}${versionOf("v")}`;
    } catch {
      return undefined;
    }
  }
  const documentPage = url.pathname.match(
    /^\/projects\/([A-Z][A-Z0-9]{1,9})\/documents\/([^/]+)\/?$/
  );
  if (!documentPage) return undefined;
  const [, project, slug] = documentPage;
  const decodedSlug = decodeURIComponent(slug ?? "");
  const document = `dispatch://${project}/artifact/${decodedSlug}`;
  const item = itemFromSearch(url.search);
  if (item === null) return undefined;
  // An ask or comment has no version; the page's version selector names the document's.
  return item === undefined
    ? `${document}${versionOf("version")}${blockOf("version")}`
    : `${document}/${item.kind}/${item.id}`;
}
/**
 * An ask, comment or message link names an item inside its issue or document, a log or
 * children link names one of its issue's lists, and a block link names one block of a document
 * or of a comment or message, or one part of an ask. `dispatch read` reads each of them and
 * `dispatch ask` cites the link in its question; the other tools that take `ref` (comment,
 * suggest, doc-edit, doc-read) take it as the owner they act on, so they would act on the owner
 * and drop the item without a word. Such a ref is refused, naming what acts on it; a part of an
 * ask, or a block of a comment or message, is named as its item.
 */
export function innerRefProblem(
  tool: string,
  ref: ParsedDispatchRef,
  refText: string
): string | undefined {
  if (
    ref.kind !== "ask" &&
    ref.kind !== "comment" &&
    ref.kind !== "message" &&
    ref.kind !== "log" &&
    ref.kind !== "children" &&
    ref.kind !== "block"
  ) {
    return undefined;
  }
  if (tool === "dispatch_read" || tool === "dispatch_ask") return undefined;
  const owner = refOwnerName(ref);
  const itemKind = ref.item?.kind ?? ref.kind;
  // suggest acts on a document and still requires an artifact, so an issue-owned link's hint
  // names one (a document block's, its own document); doc-edit and doc-read take an issue alone
  // and act on its primary document, so a document block's hint names its document for them too.
  // A comment on an issue takes the issue alone.
  const documentArgument =
    ref.kind === "block" && ref.item === undefined && tool !== "dispatch_comment"
      ? ` and artifact ${ref.id}`
      : tool === "dispatch_suggest"
        ? " and artifact <the document's slug>"
        : "";
  const ownerArguments =
    ref.owner.kind === "issue"
      ? `issue ${ref.owner.issue}${documentArgument}`
      : `project ${ref.owner.project} and artifact ${ref.artifact}`;
  // A part of an ask is what `reply_to_ask` answers, so a part's hint hands it the whole
  // reference; the ask's own id would drop the part it names. The pin goes: `reply_to_ask`
  // refuses a part pinned to a revision, since a reply answers the part whatever its text
  // becomes.
  const partRef = ref.pin === undefined ? refText : refText.slice(0, -`@v${ref.pin}`.length);
  const fix =
    tool === "dispatch_comment" && itemKind === "ask"
      ? `reply under it with reply_to_ask ${ref.item === undefined ? ref.id : partRef} and ${ownerArguments}`
      : tool === "dispatch_comment" && itemKind === "comment"
        ? `reply in its thread with reply_to ${ref.id} and ${ownerArguments}`
        : `read it with dispatch read, or name ${owner} with ${ownerArguments}`;
  const item =
    ref.kind === "ask"
      ? "an ask"
      : ref.kind === "log"
        ? "the event log"
        : ref.kind === "children"
          ? "the children"
          : ref.kind === "block"
            ? `block ${ref.block}${ref.item === undefined ? "" : ` of ${article(ref.item.kind)}`}`
            : `a ${ref.kind}`;
  return `ref ${refText} names ${item} on ${owner}; dispatch ${commandName(tool)} would act on ${owner} and drop the ${ref.kind}: ${fix}`;
}

const documentItemPattern =
  /^dispatch:\/\/([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)\/artifact\/[^/@]+(?:@v\d+)?\/(ask|comment)\/([^/]+)$/;
const versionedProjectItemPattern =
  /^dispatch:\/\/([A-Z][A-Z0-9]{1,9})\/artifact\/([^/@]+)@v\d+\/(ask|comment)\/([^/]+)$/;

/**
 * The refusal of an ask or comment reference Dispatch does not address, naming the one it does,
 * or undefined for any other value: an item on an issue's document written through the document,
 * `dispatch://KEY/artifact/<slug>/comment/<id>` (Dispatch addresses it by its issue alone, and
 * its reference grammar has no such form, so a body citing it never links), and an item on a
 * project document written at a version, which an ask or comment does not have.
 */
export function itemRefProblem(value: string): string | undefined {
  const throughDocument = value.match(documentItemPattern);
  if (throughDocument !== null) {
    const [, issue, kind, id] = throughDocument;
    return `${value} names ${kind === "ask" ? "an ask" : "a comment"} through its document, which Dispatch does not address; name it dispatch://${issue}/${kind}/${id}`;
  }
  const versioned = value.match(versionedProjectItemPattern);
  if (versioned !== null) {
    const [, project, slug, kind, id] = versioned;
    const item = kind === "ask" ? "an ask" : "a comment";
    return `${value} names ${item} at a document version, which ${item} does not have; name it dispatch://${project}/artifact/${slug}/${kind}/${id}`;
  }
  return undefined;
}
