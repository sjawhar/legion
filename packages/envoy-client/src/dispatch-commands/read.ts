import type { IssueReadOptions } from "../dispatch-http";
import {
  dispatchChildRef,
  dispatchDocumentRef,
  dispatchIssueRef,
  documentLabel,
  type ParsedDispatchRef,
} from "../dispatch-owner";
import {
  askTexts,
  datedBodies,
  eventTexts,
  type PicturesRead,
  readPictures,
} from "../dispatch-picture-tools";
import { type DatedText, picturesNewestFirst } from "../dispatch-pictures";
import { readAskRef } from "./asks";
import {
  type CommandContext,
  type DispatchToolResult,
  optionalBoolean,
  optionalNumber,
  optionalString,
  stringArg,
} from "./context";
import { approvalLine, documentListLines } from "./document-targets";
import { dispatchAnswered } from "./failures";
import { messageIdOf, refOwnerName, refTarget, refuseItemOffRef, resolveIdPrefix } from "./refs";
import {
  askPartNoun,
  blockLines,
  graphReferences,
  graphSectionLines,
  graphSections,
  issueCoverageOrReason,
  issueReferencesOrUnavailable,
  typedBlockOf,
} from "./render-graph";
import {
  childrenSummary,
  holdsSession,
  issueSummary,
  liveSessionTitles,
  logSummary,
  repositoryFieldsWarnings,
  routeHeldBySession,
} from "./render-issue";
import { askSummary, commentSummary, messageSummary } from "./render-item";

/**
 * `dispatch read`: whatever the call names — a message's conversation, an ask, a comment, a
 * message, one block, a project's documents, a document's summary, an issue's log, its children,
 * or the issue itself. Reads report their owner and follow nothing; no result subscribes the
 * session.
 */
export async function readCommand(context: CommandContext): Promise<DispatchToolResult> {
  const {
    args,
    client,
    input,
    linkBase,
    ownerArguments,
    shown,
    documentOwner,
    resolveDocument,
    issue,
  } = context;
  const message = optionalString(args, "message");
  // The pictures the shown texts embed, read beside the graph: a `Pictures:` section that
  // goes before `Referenced by:`, and the images it numbers.
  const picturesOf = (texts: readonly DatedText[]) =>
    readPictures(client, picturesNewestFirst(texts), shown);
  // The result's images. A tool result is the model's once it is returned, so they join
  // `shown` here, the last part of the result built, after every read that could fail it.
  const showImages = (pictures: PicturesRead) => {
    for (const address of pictures.shown) shown?.add(address);
    return pictures.images.length === 0 ? {} : { images: pictures.images };
  };
  if (message !== undefined) {
    const sessionId = input.sessionId?.trim();
    if (!sessionId) throw new Error("host session id is required for dispatch read --message");
    const thread = await client.getMessageThread(messageIdOf(message) as string, sessionId);
    const issueKey = thread.message.issue_key;
    const [pictures, graph] = await Promise.all([
      picturesOf(datedBodies([thread.message, ...thread.replies])),
      issueKey === null
        ? []
        : graphSections(
            client,
            dispatchChildRef(dispatchIssueRef(issueKey), "message", thread.message.id)
          ),
    ]);
    return {
      text: messageSummary(thread, [...pictures.lines, ...graph]),
      details: {
        message: thread.message.id,
        ...(issueKey === null ? {} : { issue: issueKey }),
      },
      ...showImages(pictures),
    };
  }
  // A project document's id, read once for an ask or comment its ref names: the prefix match
  // and the check that the item read is on that document share it.
  let refDocumentId: Promise<string> | undefined;
  const documentIdOf = (ref: ParsedDispatchRef) => {
    refDocumentId ??= resolveDocument(ref.owner, ref.artifact).then(
      (resolved) => resolved.artifact.id
    );
    return refDocumentId;
  };
  if (ownerArguments.ref?.kind === "ask") {
    const ref = ownerArguments.ref;
    const askRead = await readAskRef(input.tool, ref.id, refOwnerName(ref), client, async () =>
      ref.owner.kind === "issue"
        ? client.listIssueAsks(ref.owner.issue)
        : client.getArtifactAsks(await documentIdOf(ref), "all")
    );
    await refuseItemOffRef(input.tool, "ask", ref, stringArg(args, "ref"), askRead.ask, () =>
      documentIdOf(ref)
    );
    const askRef = refTarget(ref, "ask", askRead.ask.id);
    // The ask's graph is read in both directions here rather than through `graphSections`:
    // the summary lists the edges at each of its parts under that part.
    const [pictures, incoming, outgoing] = await Promise.all([
      picturesOf(askTexts(askRead)),
      graphReferences(client, { to: askRef }),
      graphReferences(client, { from: askRef }),
    ]);
    return {
      text: askSummary(askRead, askRef, pictures.lines, incoming, outgoing),
      details:
        ref.owner.kind === "project" ? { project: ref.owner.project } : { issue: ref.owner.issue },
      ...showImages(pictures),
    };
  }
  if (ownerArguments.ref?.kind === "comment") {
    const ref = ownerArguments.ref;
    const id = await resolveIdPrefix(input.tool, "comment", ref.id, refOwnerName(ref), async () =>
      ref.owner.kind === "issue"
        ? client.getComments(ref.owner.issue)
        : client.getArtifactComments(await documentIdOf(ref))
    );
    const comment = await client.getComment(id);
    await refuseItemOffRef(
      input.tool,
      "comment",
      ref,
      stringArg(args, "ref"),
      comment.comment,
      () => documentIdOf(ref)
    );
    const commentRef = refTarget(ref, "comment", id);
    const [pictures, graph] = await Promise.all([
      picturesOf(datedBodies([comment.comment, ...comment.replies])),
      graphSections(client, commentRef),
    ]);
    return {
      text: commentSummary(comment, [...pictures.lines, ...graph]),
      details:
        ref.owner.kind === "project"
          ? { project: ref.owner.project }
          : { issue: comment.comment.issue_key },
      ...showImages(pictures),
    };
  }
  if (ownerArguments.ref?.kind === "message") {
    if (ownerArguments.ref.owner.kind !== "issue") {
      throw new Error("message references are issue-scoped");
    }
    const messageRead = await client.getMessage(
      ownerArguments.ref.owner.issue,
      ownerArguments.ref.id
    );
    const messageRef = dispatchChildRef(
      dispatchIssueRef(ownerArguments.ref.owner.issue),
      "message",
      ownerArguments.ref.id
    );
    const [pictures, graph] = await Promise.all([
      picturesOf(datedBodies([messageRead.message, ...messageRead.replies])),
      graphSections(client, messageRef),
    ]);
    return {
      text: messageSummary(messageRead, [...pictures.lines, ...graph]),
      details: { issue: messageRead.message.issue_key },
      ...showImages(pictures),
    };
  }
  if (ownerArguments.ref?.kind === "block") {
    const ref = ownerArguments.ref;
    const blockRef = stringArg(args, "ref");
    // The document, ask, comment or message the block is in: the reference before its `#b-`.
    const holder = blockRef.slice(0, blockRef.indexOf("#"));
    // A part of an ask is named by what it is — the question, an option or the answer — and
    // no node carries a part's type, so the ask's own read is read beside the graph. That
    // read only chooses the heading's word, so it is advisory: a failure of it leaves the
    // part under the general `Part` heading and the rest of the read unchanged.
    const askOfPart =
      ref.item?.kind === "ask" ? client.getAsk(ref.item.id).catch(() => undefined) : undefined;
    // The read names the block's node, so a 404 is the block's own answer: an unpinned ref
    // names a block no current version holds, and a pinned one's answer says which of its
    // version, that version's record or the block the server could not find.
    const [incoming, outgoing, partOf, typed] = await Promise.all([
      client.getReferences({ to: blockRef }).catch((error: unknown) => {
        if (ref.pin === undefined && dispatchAnswered(error, 404)) {
          throw new Error(`No block ${ref.block} in ${holder}.`);
        }
        throw error;
      }),
      graphReferences(client, { from: blockRef }),
      askOfPart,
      // The live block of its document, for its type and the attributes it carries: a block
      // of a comment or a message has neither, and so has one no live version holds.
      typedBlockOf(client, ref),
    ]);
    const blockID = incoming.node.block_id ?? ref.block ?? "";
    // A part whose ask the read could not name reads as a `Part`, as a part the ask does not
    // place does.
    const noun =
      ref.item?.kind !== "ask"
        ? undefined
        : partOf === undefined
          ? "Part"
          : askPartNoun(partOf.ask, incoming.node, blockID);
    return {
      text: [
        ...blockLines(incoming.node, ref, typed, noun),
        ...graphSectionLines(incoming, outgoing),
      ].join("\n"),
      details: {
        ...(ref.owner.kind === "project"
          ? { project: ref.owner.project }
          : { issue: ref.owner.issue }),
        block: incoming.node.block_id ?? ref.block,
      },
    };
  }
  // A project named with no document reads as the list of its documents (those on no issue,
  // the ones a project document reference names), which a project document reference's
  // not-found hint points to.
  const projectOwner = documentOwner();
  if (projectOwner.kind === "project" && optionalString(args, "artifact") === undefined) {
    const documents = await client.listProjectArtifacts(projectOwner.project, true);
    return {
      text: [
        `Project: ${projectOwner.project}`,
        "Documents:",
        ...documentListLines(documents),
      ].join("\n"),
      details: { project: projectOwner.project },
    };
  }
  // A document's summary. An issue's document reads as a project's does, with the issue it
  // belongs to, so a document named by artifact, by ref, or by a search hit's link is the
  // thing read rather than its issue.
  const documentReference = optionalString(args, "artifact");
  if (documentOwner().kind === "project" || documentReference !== undefined) {
    const resolved = await resolveDocument(documentOwner(), documentReference);
    const { artifact } = resolved;
    const ownerKey = resolved.issue?.key ?? artifact.project;
    const documentRef = dispatchDocumentRef(ownerKey, artifact.slug);
    const approval = approvalLine(artifact);
    return {
      text: [
        `Document: ${ownerKey} / ${artifact.name}`,
        ...(resolved.issue === undefined
          ? []
          : [`Issue: ${resolved.issue.key} [${resolved.issue.status}] ${resolved.issue.title}`]),
        `Reference: ${documentRef}`,
        `Versions: ${artifact.versions.length}`,
        ...(approval === undefined ? [] : [approval]),
        ...(await graphSections(client, documentRef)),
      ].join("\n"),
      details: {
        ...(resolved.issue === undefined
          ? { project: artifact.project }
          : { issue: resolved.issue.key }),
        document: documentLabel(ownerKey, artifact.slug),
      },
    };
  }
  const issueKey = issue();
  // Which of the issue's events the issue read and its log list; `argumentProblems` refused a
  // count beside any other read.
  const readEvents: IssueReadOptions = {
    events: optionalBoolean(args, "all_events") === true ? "all" : optionalNumber(args, "events"),
  };
  if (ownerArguments.ref?.kind === "log") {
    const read = await client.read(issueKey, readEvents);
    const pictures = await picturesOf(read.events.flatMap(eventTexts));
    return {
      text: [logSummary(read.issue, read.events), ...pictures.lines].join("\n"),
      details: { issue: read.issue.key },
      ...showImages(pictures),
    };
  }
  if (ownerArguments.ref?.kind === "children") {
    // The children list prints no events, so only the issue is read.
    const parent = await client.getIssue(issueKey);
    return {
      text: childrenSummary(parent),
      details: { issue: parent.key },
    };
  }
  const readPromise = client.read(issueKey, readEvents);
  // Start the independent, non-fatal closure lookup with the issue read. `read` retains
  // its internal issue-then-events order because event pagination starts at issue.last_seq.
  const referencesPromise = issueReferencesOrUnavailable(client, issueKey);
  const read = await readPromise;
  // The claim's holder must read the same here as on the issue page, so the label comes
  // from the live registry — asked for only when a session holds this issue.
  const [references, graph, titles, pictures, coverage] = await Promise.all([
    referencesPromise,
    graphSections(client, dispatchIssueRef(read.issue.key)),
    liveSessionTitles(client, holdsSession(read.issue.claim) || routeHeldBySession(read.issue)),
    picturesOf([
      ...read.issue.open_asks.map((ask) => ({ text: ask.question, at: ask.created_at })),
      ...read.events.flatMap(eventTexts),
    ]),
    issueCoverageOrReason(client, read.issue.key),
  ]);
  return {
    text: issueSummary(
      read.issue,
      read.events,
      references,
      [...pictures.lines, ...graph],
      linkBase,
      coverage,
      titles
    ),
    details: { issue: read.issue.key },
    ...showImages(pictures),
    ...repositoryFieldsWarnings(read.issue),
  };
}
