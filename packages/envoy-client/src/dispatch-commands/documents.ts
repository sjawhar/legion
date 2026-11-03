import { resolve as resolvePath } from "node:path";
import type { Artifact, ArtifactBlock, Ask, EditOp, EditPrecondition } from "@legion/contracts";
import { typedBlockIdAttribute } from "@legion/contracts";
import { commandName } from "../dispatch-command";
import type { DispatchClient } from "../dispatch-http";
import {
  dispatchChildRef,
  dispatchDocumentRef,
  dispatchIssueRef,
  documentLabel,
  issueTopic,
  parseDispatchRef,
} from "../dispatch-owner";
import { shownPictureLine } from "../dispatch-picture-tools";
import {
  oversizePictureText,
  PICTURE_SHOWN_MAX_BYTES,
  sniffPictureType,
  toolImage,
} from "../dispatch-pictures";
import { decisionBlockLines } from "../related-check";
import { otherAdviceLines, renderAdvice } from "./advice";
import {
  type CommandContext,
  type DispatchToolResult,
  optionalBoolean,
  optionalNumber,
  optionalString,
  type ResolvedArtifact,
  stringArg,
} from "./context";
import { approvalLine, liveBlocks } from "./document-targets";
import { openAnchoredThreads } from "./render-item";
import {
  documentResultDetails,
  documentTopic,
  followedAskDetails,
  notSubscribed,
  resolvedTopic,
  writeResultDetails,
} from "./results";

/** The asks of the document's `ask` blocks. An issue lists them under the issue, since the artifact
 * route refuses an issue document; an unlinked document lists its own. */
async function blockAsks(
  client: DispatchClient,
  resolved: ResolvedArtifact,
  state?: "all" | "open" | "answered"
): Promise<Array<Ask & { readonly block_id: string }>> {
  const asks = await (resolved.issue === undefined
    ? client.getArtifactAsks(resolved.artifact.id, state)
    : client.listIssueAsks(resolved.issue.key, state));
  return asks.filter(
    (ask): ask is Ask & { readonly block_id: string } =>
      typeof ask.block_id === "string" && ask.block_artifact?.id === resolved.artifact.id
  );
}
/**
 * dispatch_doc_read of an uploaded file or image. Dispatch serves a file's content only as the
 * bytes of one of its versions (its `/text` route answers a file 400 NOT_DOCUMENT), so this reads
 * the version asked for, or the latest. A picture a model takes (an image artifact whose bytes are
 * a PNG, JPEG, GIF or WebP of at most `PICTURE_SHOWN_MAX_BYTES`) comes back as an image block with
 * its name, type, version and size as the text, then the line naming it by its address
 * (`dispatch://<owner>/artifact/<slug>@v<N>`, `owner` an issue key, a project key or
 * `agent/<session id>`) as `dispatch_read` does, which a host reads back from a transcript; a
 * larger picture is described without its bytes being fetched when Dispatch states its size. Any
 * other file comes back as text when it is UTF-8, and is described with the route that serves its
 * bytes when it is not. A file carries no token, anchors or approval, so nothing else is read. A
 * picture is shown whatever `shown` holds, and joins it under its address, so the session's later
 * reads name it instead of sending it.
 */
async function readUploadedFile(
  client: DispatchClient,
  artifact: Artifact,
  details: Record<string, unknown>,
  requested: number | undefined,
  owner: string,
  shown: Set<string> | undefined
): Promise<DispatchToolResult> {
  const latest = Math.max(0, ...artifact.versions.map((version) => version.number));
  const number = requested ?? latest;
  const of = number === latest ? "" : ` of ${latest}`;
  const bytesRoute = `GET /api/v1/artifacts/${artifact.id}/versions/${number} serves its bytes.`;
  const tooLarge = (mime: string, bytes: number): DispatchToolResult => ({
    text:
      `${oversizePictureText(artifact.name, bytes)}, so dispatch doc-read cannot show this ` +
      `uploaded ${mime} picture (version ${number}${of}). ${bytesRoute}`,
    details,
  });
  const stated = artifact.versions.find((version) => version.number === number);
  if (
    artifact.kind === "image" &&
    stated?.size !== undefined &&
    stated.size > PICTURE_SHOWN_MAX_BYTES
  ) {
    return tooLarge(stated.mime ?? "image", stated.size);
  }
  const file = await client.fileVersion(artifact.id, number);
  const size = `${file.bytes.length.toLocaleString("en-US")} bytes`;
  if (artifact.kind === "image") {
    const image = toolImage(file.bytes);
    if (image !== undefined) {
      const address = `dispatch://${owner}/artifact/${artifact.slug}@v${number}`;
      shown?.add(address);
      return {
        text:
          `Picture ${artifact.name}: ${image.mimeType}, version ${number}${of}, ${size}.\n` +
          shownPictureLine(1, address, artifact.name, image.mimeType, file.bytes.length),
        details,
        images: [image],
      };
    }
    if (sniffPictureType(file.bytes) !== undefined) return tooLarge(file.mime, file.bytes.length);
  }
  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(file.bytes);
  } catch {
    return {
      text:
        `${artifact.name} is an uploaded ${file.mime} file (version ${number}${of}, ${size}) that is not ` +
        `UTF-8 text, so dispatch doc-read cannot show it. ${bytesRoute}`,
      details,
    };
  }
  return {
    text: `File ${artifact.name}: ${file.mime}, version ${number}${of}, ${size}.\n\n${text}`,
    details,
  };
}

/** How many characters of a block's text `dispatch doc-read --blocks` shows: enough to tell blocks
 *  apart without printing the document twice. */
const BLOCK_OPENING_CHARS = 60;

/**
 * `dispatch doc-read --blocks`: the live document's blocks in document order, one line each, a
 * block indented under the block whose markdown range holds it. Each line carries the id, type,
 * token and anchor counts `/blocks` reports, then the block's opening words, sliced from the live
 * markdown by the block's byte range (Dispatch reports UTF-8 offsets into `/text`'s markdown). A
 * table lists its rows and cells from its `/blocks` entry, the indexes `delete_row` and
 * `delete_column` take placed by Dispatch from its tree; a Dispatch whose entry carries no rows is
 * said to report none, never answered from the markdown, since a column read from the markdown is
 * not the index `delete_column` takes once a cell spans columns. The markdown is read before and
 * after the blocks, and a document whose token or text moved between them is refused, since the
 * offsets would slice text they were not computed over.
 */
async function listDocumentBlocks(
  client: DispatchClient,
  artifact: Artifact
): Promise<{ text: string; token: string | undefined }> {
  const live = await liveBlocks(client, artifact);
  if (live === undefined) {
    throw new Error(
      `${artifact.name} changed while dispatch doc-read --blocks read it, so the opening words could name the wrong text; nothing is listed. Run it again.`
    );
  }
  const { bytes, blocks } = live;
  const decoder = new TextDecoder();
  const lines = [
    `Blocks of ${artifact.name}, in document order, each indented under the block holding it: id, type, (token; anchored comments and asks), opening words. A table lists its rows by the index delete_row takes and its cells by the row and the column delete_column takes.`,
  ];
  const holders: ArtifactBlock[] = [];
  for (const block of blocks) {
    while (holders.length > 0 && block.from >= (holders.at(-1)?.to ?? 0)) holders.pop();
    const indent = "  ".repeat(holders.length);
    const flat = decoder.decode(bytes.subarray(block.from, block.to)).replace(/\s+/g, " ").trim();
    // Cut by code point, never inside a surrogate pair, which would print U+FFFD.
    const points = Array.from(flat);
    const opening =
      points.length > BLOCK_OPENING_CHARS
        ? `${points.slice(0, BLOCK_OPENING_CHARS).join("")}…`
        : flat;
    const { comments, asks } = block.references;
    lines.push(
      `${indent}${block.id} ${block.type} (${block.token ?? "no token"}; ${comments} comment${comments === 1 ? "" : "s"}, ${asks} ask${asks === 1 ? "" : "s"}): ${opening}`
    );
    if (block.type === "table") {
      if (block.table === undefined) {
        lines.push(
          `${indent}  This Dispatch does not report table rows yet, so no row or column index is listed.`
        );
      } else {
        for (const row of block.table.rows) {
          lines.push(
            `${indent}  row ${row.index}, ${row.id === "" ? "no id yet" : `id ${row.id}`}`
          );
          for (const cell of row.cells) {
            const under = cell.header === "" ? "" : `, under ${JSON.stringify(cell.header)}`;
            lines.push(
              `${indent}    row ${cell.row} column ${cell.column}, ${cell.id === "" ? "no id yet" : `id ${cell.id}`}${under}: ${cell.text === "" ? "(empty)" : cell.text}`
            );
          }
        }
      }
    }
    holders.push(block);
  }
  return { text: lines.join("\n"), token: live.token };
}
/**
 * Refuses an approval request while the document holds an open decision block. A request names
 * the latest version, and a new version moves it to that version and leaves it waiting on its
 * agent, so a request over a block the human has yet to answer leaves their turn the moment they
 * answer it. The live document's `ask` blocks are judged by the latest version, the one the
 * request would name: a block that version shows open counts as open even when its ask is already
 * answered or closed, since that answer reaches a version only when the document settles, about
 * two seconds later, or with the agent's next edit, whichever is first. An answered block already
 * carries its own record, so no edit is needed for that; a waiver carries none, so the agent's
 * next edit is where the human's decision gets written into the text. A block not yet in that
 * version counts as open too. A block removed from the document is not judged here;
 * `refuseRemovingOpenDecisionBlocks` keeps one whose ask is open in it. A document already
 * approved at its latest version is left to the server, which answers with that approval.
 */
async function refuseOpenDecisionBlocks(
  client: DispatchClient,
  tool: string,
  resolved: ResolvedArtifact
): Promise<void> {
  const artifact = resolved.artifact;
  const latest = artifact.approval?.latest_version;
  // No approval state means no document version to approve: the server's own refusal says so.
  if (latest === undefined || latest < 1 || artifact.approval?.state === "approved") return;
  const blocks = (await client.artifactBlocks(artifact.id)).filter((block) => block.type === "ask");
  if (blocks.length === 0) return;
  const [documentAsks, version] = await Promise.all([
    blockAsks(client, resolved),
    client.docRead(artifact.id, latest),
  ]);
  const asks = new Map(documentAsks.map((ask) => [ask.block_id, ask]));
  const lines = version.markdown.split("\n");
  // An answered block already carries its own record and settles into a version on its own; a
  // waived one (resolved) gets the decision written in.
  const stillOpen = (named: string, state: string): string =>
    state === "answered"
      ? `${named}, answered but still open in version ${latest}: it settles into a version on its own in about two seconds; wait for that version`
      : `${named}, ${state} but still open in version ${latest}: write the decision into the text with dispatch doc-edit, which writes a version that carries it`;
  const open = blocks.flatMap((block): string[] => {
    const ask = asks.get(block.id);
    // The block's opening lines, `:::ask{<id> … state="…"}`, the id as the renderer writes it
    // (`typedBlockIdAttribute`: `#<id>`, or `id="…"` for an id the shortcut cannot hold). Every
    // match counts, so a line that quotes the opener (in code, say) can add an open block but
    // never hide one; a block none of whose lines carries a state is open.
    const id = typedBlockIdAttribute(block.id);
    const openers = lines.filter(
      (line) => line.includes(`ask{${id} `) || line.includes(`ask{${id}}`)
    );
    const states = openers.map((line) => /\bstate="(\w+)"/.exec(line)?.[1]);
    const named =
      ask === undefined
        ? `block ${block.id}`
        : `${JSON.stringify(ask.question)} (block ${block.id}, ask ${ask.id})`;
    if (states.length === 0) return [`${named}, which version ${latest} does not hold yet`];
    if (!states.includes("open") && states.some((state) => state !== undefined)) return [];
    if (ask !== undefined) return ask.state === "open" ? [named] : [stillOpen(named, ask.state)];
    // A block copied from another document's ask opens none: settlement names that ask in
    // `copied_from`, its document in `copied_from_document`, and writes its state into the block,
    // which this version shows open, so the source is where it is answered. The document is named
    // as the dashboard's decision card names it, its issue key and slug or its project's
    // `<project>/<slug>`, so the ask's address is the line's one link.
    const attribute = (name: string): string | undefined =>
      openers
        .map((line) => new RegExp(`\\b${name}="([^"]+)"`).exec(line)?.[1])
        .find((value) => value !== undefined);
    const sourceId = attribute("copied_from");
    const document = attribute("copied_from_document");
    if (sourceId === undefined || document === undefined) {
      return [`${named}, whose ask Dispatch has not opened yet`];
    }
    const source = parseDispatchRef(document);
    const where =
      source === null
        ? document
        : source.owner.kind === "project"
          ? documentLabel(source.owner.project, source.id)
          : `${source.owner.issue} ${source.id}`;
    const ref =
      source?.owner.kind === "issue"
        ? dispatchChildRef(dispatchIssueRef(source.owner.issue), "ask", sourceId)
        : dispatchChildRef(document, "ask", sourceId);
    return [`${named}, a copy of ask ${ref}, which is open on ${where}: answer it there`];
  });
  if (open.length === 0) return;
  const count = open.length === 1 ? "1 open decision block" : `${open.length} open decision blocks`;
  throw new Error(
    [
      `dispatch ${commandName(tool)} was not called: ${artifact.name} (version ${latest}) has ${count}. Answering one writes a new version, which would move this request to that version and leave it waiting on you.`,
      ...open.map((line) => `- ${line}`),
      "Do not request approval over an open block, even when a human asked for it. Tell the human which block is open and ask them to answer it or to waive it. Once it is answered, it settles into a version on its own in about two seconds. When the human approved an earlier version and their answer is the whole change since it, Dispatch carries the approval to the new version in their name: request nothing. When the answer changes design text outside the block, rewrite that text (never to restate the answer), then request approval again once the human has agreed to every point in the new version, with a one-sentence --summary naming what changed since the earlier approval. On a document nobody has approved, wait for that version, then request. If they waive it, close the block with dispatch resolve-ask (--kind resolved, their words as the --reason), write their decision into the text with dispatch doc-edit, then request approval again once the human has agreed to every point in the new version. The call opens the request, or hands an open one back to the human.",
    ].join("\n")
  );
}

/**
 * Refuses a document edit that would take a decision block out of the document while its ask is
 * open: a `delete` of the block or of a block holding it, or a `retype` of it into another type.
 * The edit writes its version at once; about two seconds later settlement retracts the ask in the
 * system's name and writes no version, so the human's question leaves the Inbox unanswered and an
 * approval request made after it names a version with no open block for
 * `refuseOpenDecisionBlocks` to find. An `insert` in the same batch that carries the block's id
 * does not exempt it: telling a block written back from an opener quoted in code needs the
 * server's parser, so the executor fails closed, as `refuseOpenDecisionBlocks` does for a quoted
 * opener. An open block is reworded with `replace`, moved with `move`, or, when this session asked
 * it, has its question, options, urgency or multiple changed with `dispatch_edit_ask`; each keeps
 * it. Costs nothing for an edit with no such operation, then one
 * `GET /artifacts/{id}/blocks`, and the owner's asks only when an operation reaches an `ask` block.
 */
async function refuseRemovingOpenDecisionBlocks(
  client: DispatchClient,
  tool: string,
  resolved: ResolvedArtifact,
  ops: readonly EditOp[]
): Promise<void> {
  const removing = ops.filter(
    (operation) =>
      operation.block !== undefined &&
      (operation.op === "delete" || (operation.op === "retype" && operation.type !== "ask"))
  );
  if (removing.length === 0) return;
  const artifact = resolved.artifact;
  const blocks = await client.artifactBlocks(artifact.id);
  const askBlocks = blocks.filter((block) => block.type === "ask");
  const removed = new Set<string>();
  for (const operation of removing) {
    const target = blocks.find((block) => block.id === operation.block);
    if (target === undefined) continue;
    // A delete takes every block inside the one it names; a retype changes only that block.
    for (const block of askBlocks) {
      if (
        block.id === target.id ||
        (operation.op === "delete" && block.from >= target.from && block.to <= target.to)
      ) {
        removed.add(block.id);
      }
    }
  }
  if (removed.size === 0) return;
  const asks = await blockAsks(client, resolved, "open");
  const open = asks.filter((ask) => removed.has(ask.block_id));
  if (open.length === 0) return;
  const [what, question] =
    open.length === 1
      ? ["a decision block whose ask is", "question"]
      : [`${open.length} decision blocks whose asks are`, "questions"];
  throw new Error(
    [
      `dispatch ${commandName(tool)} was not called: it would remove ${what} still open, and the human's ${question} would leave their Inbox unanswered.`,
      ...open.map(
        (ask) => `- ${JSON.stringify(ask.question)} (block ${ask.block_id}, ask ${ask.id})`
      ),
      "A decision block leaves the document once its ask is answered or resolved. Until then, reword it with replace, relocate it with move, or change its question, options, urgency or multiple with dispatch edit-ask if you asked it; each keeps it.",
    ].join("\n")
  );
}

/** `dispatch doc-edit`: apply one batch of block operations to a live document. */
export async function docEditCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, linkBase, documentOwner, resolveDocument } = context;
  const resolved = await resolveDocument(documentOwner(), optionalString(args, "artifact"));
  const ops = args.ops as EditOp[];
  const summary = optionalString(args, "summary");
  const { precondition: rawPrecondition } = args;
  const precondition = rawPrecondition as EditPrecondition | undefined;
  await refuseRemovingOpenDecisionBlocks(client, input.tool, resolved, ops);
  const edited = await client.docEdit(resolved.artifact.id, {
    ops,
    ...(summary === undefined ? {} : { summary }),
    ...(precondition === undefined ? {} : { precondition }),
    actor,
  });
  const retyped = ops.filter((operation) => operation.op === "retype").length;
  const versionText =
    edited.version === null ? "no new version" : `version ${edited.version.number}`;
  const retypedText = retyped === 0 ? "" : `; retyped ${retyped} block${retyped === 1 ? "" : "s"}`;
  // A batch that left the document as it was mints no version: say so, and name the
  // operations that did nothing, so the next attempt is aimed at the quote, not the version.
  const nothingChanged = edited.changed === false;
  const head = nothingChanged
    ? `Applied ${edited.applied} ops${retypedText}; nothing changed (${versionText})`
    : `Applied ${edited.applied} ops${retypedText} (${versionText})`;
  const unchangedOps = edited.unchanged_ops ?? [];
  const unchangedText =
    unchangedOps.length === 0
      ? ""
      : `; ${unchangedOps.length === 1 ? "operation" : "operations"} ${unchangedOps.join(", ")} changed nothing`;
  // A change the live document no longer carries: a browser deletion that landed after this
  // edit's version was rendered and before it reached the room, which is past undoing, so the
  // version records text the live document does not have (LEGION-269). `null` is a check that
  // reached no verdict - the room is reloading, or holds a tree past the schema's depth bound,
  // which a re-read answers DOC_SCHEMA for; an older server omits the field and reads as it
  // always did.
  const lostOps = edited.lost_ops;
  const lostText =
    lostOps === undefined || (lostOps !== null && lostOps.length === 0)
      ? ""
      : lostOps === null
        ? "; could not confirm this edit survived, because the live document is being reloaded or holds a tree too deep to read — re-read it"
        : `; ${versionText} carries text the live document no longer has: a concurrent change removed what ${lostOps.length === 1 ? "operation" : "operations"} ${lostOps.join(", ")} wrote — re-read the document`;
  const applied = `${head}${unchangedText}${lostText}`;
  // An older server names no block, so the one not-made line it gets takes its search words
  // from the first ask block this edit's own markdown opens (its question comes first), else
  // from the document's issue title; a retype on a project document has neither, and the
  // line asks for the words instead.
  const editAskText =
    ops
      .map(
        (operation) =>
          /^:{3,}ask\{.*\n([\s\S]*)/mu.exec(operation.markdown ?? operation.with ?? "")?.[1]
      )
      .find((text) => text !== undefined) ??
    resolved.issue?.title ??
    "";
  const checkLines = decisionBlockLines(edited.advice, {
    project: resolved.artifact.project,
    text: editAskText,
    configUrl: linkBase,
  });
  const adviceLines = await otherAdviceLines(() =>
    renderAdvice(input.tool, resolvedTopic(resolved).label, edited.advice, {})
  );
  // The token of the document this edit produced, rendered as dispatch_doc_read renders it:
  // the next guarded edit passes it as `precondition.document` with no read in between. A
  // Dispatch server predating it returns none, and the result reads as it always did.
  const tokenTrailer = edited.token === undefined ? [] : [`Document token: ${edited.token}`];
  return {
    text: [
      `${applied}${await notSubscribed(resolvedTopic(resolved), input.subscriptions)}`,
      ...checkLines,
      ...tokenTrailer,
      ...adviceLines,
    ].join("\n"),
    details: writeResultDetails(resolved, {
      applied: edited.applied,
      ...(edited.version === null ? {} : { version: edited.version.number }),
      ...(nothingChanged ? { changed: false } : {}),
      ...(lostOps === undefined ? {} : { lost_ops: lostOps }),
      ...(edited.token === undefined ? {} : { token: edited.token }),
      ...(edited.advice === undefined ? {} : { advice: edited.advice }),
    }),
  };
}

/** `dispatch doc-read`: a document's markdown, its blocks, or an uploaded file's content. */
export async function docReadCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, ownerArguments, shown, documentOwner, resolveDocument } = context;
  const { agentArtifact } = ownerArguments;
  if (agentArtifact !== undefined) {
    // A conversation's upload is a file or picture; Dispatch keeps no document there.
    const artifact = await client.getAgentArtifact(agentArtifact.session, agentArtifact.slug);
    if (optionalBoolean(args, "blocks") === true) {
      throw new Error(
        `${artifact.name} is an uploaded ${artifact.kind}, not a document, so it has no blocks. Read it with dispatch doc-read without --blocks.`
      );
    }
    return readUploadedFile(
      client,
      artifact,
      { session: agentArtifact.session, artifact: artifact.slug },
      optionalNumber(args, "version") ?? agentArtifact.version,
      `agent/${agentArtifact.session}`,
      shown
    );
  }
  const artifactReference =
    optionalString(args, "artifact") ??
    (ownerArguments.ref?.kind === "spec" || ownerArguments.ref?.kind === "artifact"
      ? ownerArguments.ref.id
      : undefined);
  const resolved = await resolveDocument(documentOwner(), artifactReference);
  const version = optionalNumber(args, "version") ?? ownerArguments.ref?.version;
  if (optionalBoolean(args, "blocks") === true) {
    if (resolved.artifact.kind === "file" || resolved.artifact.kind === "image") {
      throw new Error(
        `${resolved.artifact.name} is an uploaded ${resolved.artifact.kind}, not a document, so it has no blocks. Read it with dispatch doc-read without --blocks.`
      );
    }
    const listed = await listDocumentBlocks(client, resolved.artifact);
    return {
      text:
        listed.token === undefined
          ? listed.text
          : `${listed.text}\n\nDocument token: ${listed.token}`,
      details:
        resolved.owner.kind === "project"
          ? {
              project: resolved.artifact.project,
              document: documentLabel(resolved.artifact.project, resolved.artifact.slug),
            }
          : { issue: resolved.issue?.key },
    };
  }
  if (resolved.artifact.kind === "file" || resolved.artifact.kind === "image") {
    const { artifact } = resolved;
    return readUploadedFile(
      client,
      artifact,
      resolved.owner.kind === "project"
        ? {
            project: artifact.project,
            document: documentLabel(artifact.project, artifact.slug),
          }
        : { issue: resolved.issue?.key },
      version,
      resolved.owner.kind === "project" ? resolved.owner.project : resolved.owner.issue,
      shown
    );
  }
  const documentPromise = client.docRead(resolved.artifact.id, version);
  const threadsPromise = openAnchoredThreads(client, resolved);
  const threadsResultPromise = threadsPromise.then(
    (value) => ({ status: "fulfilled" as const, value }),
    (reason) => ({ status: "rejected" as const, reason })
  );
  // A document error takes precedence over a thread error. Capture the concurrent thread
  // failure so it is handled without delaying a document failure.
  const document = await documentPromise;
  const threadsResult = await threadsResultPromise;
  if (threadsResult.status === "rejected") throw threadsResult.reason;
  const threads = threadsResult.value;
  const approval = approvalLine(resolved.artifact);
  const trailer = [
    ...("token" in document && document.token !== undefined
      ? [`Document token: ${document.token}`]
      : []),
    ...(approval === undefined ? [] : [approval]),
  ];
  return {
    text: [
      document.markdown,
      ...(trailer.length === 0 ? [] : [trailer.join("\n")]),
      ...(threads.length === 0 ? [] : ["Open anchored threads:", ...threads]),
    ].join("\n\n"),
    details:
      resolved.owner.kind === "project"
        ? {
            project: resolved.artifact.project,
            document: `${resolved.artifact.project}/${resolved.artifact.slug}`,
          }
        : { issue: resolved.issue?.key },
  };
}

/** `dispatch request-approval`: ask the human to approve this version of a document. */
export async function requestApprovalCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, ownerArguments, documentOwner, resolveDocument } = context;
  const artifactReference =
    optionalString(args, "artifact") ??
    (ownerArguments.ref?.kind === "spec" || ownerArguments.ref?.kind === "artifact"
      ? ownerArguments.ref.id
      : undefined);
  const resolved = await resolveDocument(documentOwner(), artifactReference);
  await refuseOpenDecisionBlocks(client, input.tool, resolved);
  const result = await client.requestApproval(resolved.artifact.id, {
    actor,
    summary: stringArg(args, "summary"),
  });
  if (result.ask === null) {
    return {
      text: `${resolved.artifact.name} (document id ${resolved.artifact.id}) is already approved at version ${result.version} by ${result.approval.by?.id ?? "unknown"}; no new request was opened. A change outside the decision blocks after approval makes it stale, so request again only for such a version, once the human has agreed to every point in it, with a --summary naming what changed since version ${result.version}; the human's own answer to a decision block carries the approval when that answer is the whole change.`,
      details: {
        ...(resolved.owner.kind === "project"
          ? documentResultDetails(resolved.artifact)
          : { issue: resolved.issue?.key }),
        artifact: resolved.artifact.id,
        version: result.version,
      },
    };
  }
  const details = await followedAskDetails(client, result.ask, resolved.artifact);
  // A call that opened, reworded or handed back the request says so; one that found it already
  // waiting on the human says nothing changed, so a retry never reads as a fresh hand-back.
  const outcome = result.recorded
    ? `Approval requested for ${resolved.artifact.name} (document id ${resolved.artifact.id}) at version ${result.version} (ask ${result.ask.id}).`
    : `The approval request for ${resolved.artifact.name} (document id ${resolved.artifact.id}) at version ${result.version} (ask ${result.ask.id}) already waits on the human, so this call changed nothing: nothing since it last reached the human (a newer version, a human's reply in its thread, or your progress note) left it waiting on you.`;
  return {
    text: `${outcome} The human's Inbox asks: ${JSON.stringify(result.ask.question)}. The answer arrives as artifact.approved or artifact.changes_requested. An edit before the answer moves this request to the new version and leaves it waiting on you, and a change outside the decision blocks after approval makes the approval stale: either way, request again for the new version once the human has agreed to every point in it, which hands this request back or opens a new one, with a --summary naming what changed since the earlier approval. The human's own answer to a decision block carries the approval when that answer is the whole change since the approved version, and answers an open request Approve in their name: request nothing then.`,
    details: { ...details, artifact: resolved.artifact.id, version: result.version },
  };
}

/** `dispatch artifact`: upload a document, a picture or a file to an issue or a project. */
export async function artifactCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, documentOwner, issue } = context;
  const summary = optionalString(args, "summary");
  const name = stringArg(args, "name");
  const content = optionalString(args, "content");
  const artifactInput =
    content === undefined
      ? {
          name,
          // The bytes AskGate's check read, when it read them, typed as Bun types the path.
          file:
            input.uploadBytes === undefined
              ? Bun.file(resolvePath(input.cwd, stringArg(args, "path")))
              : // The copy gives the bytes a plain buffer of their own, which a Blob takes.
                new Blob([new Uint8Array(input.uploadBytes)], {
                  type: Bun.file(resolvePath(input.cwd, stringArg(args, "path"))).type,
                }),
          ...(summary === undefined ? {} : { summary }),
          actor,
        }
      : {
          name,
          content,
          ...(summary === undefined ? {} : { summary }),
          actor,
        };
  const artifactOwner = documentOwner();
  const result =
    artifactOwner.kind === "project"
      ? await client.projectArtifact(artifactOwner.project, artifactInput)
      : await client.artifact(issue(), artifactInput);
  const artifactRef = dispatchDocumentRef(
    artifactOwner.kind === "project" ? artifactOwner.project : issue(),
    result.artifact.slug
  );
  const uploadOwner =
    artifactOwner.kind === "project" ? documentTopic(result.artifact) : issueTopic(issue());
  const adviceLines = renderAdvice(input.tool, uploadOwner.label, result.advice, {
    isPrimarySpec: result.artifact.primary || result.artifact.name === "spec.md",
  });
  return {
    text: [
      `Uploaded ${result.artifact.name} as version ${result.version.number} (artifact slug ${result.artifact.slug}; ${artifactRef})${await notSubscribed(uploadOwner, input.subscriptions)}`,
      ...adviceLines,
    ].join("\n"),
    details:
      artifactOwner.kind === "project"
        ? {
            ...documentResultDetails(result.artifact),
            version: result.version.number,
            ...(result.advice === undefined ? {} : { advice: result.advice }),
          }
        : {
            issue: issue(),
            artifact: result.artifact.id,
            version: result.version.number,
            ...(result.advice === undefined ? {} : { advice: result.advice }),
          },
  };
}
