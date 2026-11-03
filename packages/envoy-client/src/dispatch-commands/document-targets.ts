import type { Artifact, ArtifactBlock } from "@legion/contracts";
import { commandLine } from "../dispatch-command";
import { defaultExec } from "../dispatch-cwd";
import type { DispatchClient } from "../dispatch-http";
import type { Owner } from "../dispatch-owner";
import { ToolInputError } from "../tool-input-errors";
import { ownerConflictProblems, resolveOwnerArguments } from "./check";
import {
  type DispatchCallCheckInput,
  type OwnerResolution,
  optionalNumber,
  optionalString,
  type ResolvedArtifact,
  stringArg,
  type ToolArguments,
} from "./context";
import { dispatchAnswered } from "./failures";

/** A document reference that names no document of its owner. Its message is the hint the Dispatch
 * tools give; `readDocumentBlocks` tells it apart from every other refusal, as the first upload of
 * a file. */
class DocumentNotFoundError extends Error {}

/** The document `artifactReference` names among `artifacts`. A bare reference is an id, a slug,
 * then a filename; a `canonical` one, the document part of a dispatch:// reference, is a slug by
 * definition (the address the dashboard and Dispatch's own routes use), so its slug match comes
 * before an id. */
function artifactByReference(
  artifacts: readonly Artifact[],
  artifactReference: string,
  owner: Owner,
  canonical: boolean
): Artifact {
  const bySlug = artifacts.find((candidate) => candidate.slug === artifactReference);
  if (canonical && bySlug !== undefined) return bySlug;
  const byId = artifacts.find((candidate) => candidate.id === artifactReference);
  if (byId !== undefined) return byId;
  // A slug names one document, and wins over another document's filename, as on Dispatch's own
  // routes (resolveDocumentReference): an older Dispatch could store a document whose filename is
  // another's slug (`plan-md-2` named `plan-md` beside `plan.md`), and Dispatch reads and edits the
  // slug's document by that reference. Only a filename two documents share names neither.
  if (bySlug !== undefined) return bySlug;
  const byName = artifacts.filter((candidate) => candidate.name === artifactReference);
  if (byName.length > 1) {
    throw new Error(documentReferenceProblem(artifactReference, byName, owner, true));
  }
  const [artifact] = byName;
  if (artifact === undefined) {
    throw new DocumentNotFoundError(documentReferenceProblem(artifactReference, artifacts, owner));
  }
  return artifact;
}

export async function resolveArtifact(
  client: DispatchClient,
  owner: Owner,
  artifactReference: string | undefined,
  { canonical = false }: { readonly canonical?: boolean } = {}
): Promise<ResolvedArtifact> {
  if (owner.kind === "project") {
    if (artifactReference === undefined) {
      throw new Error("artifact is required for a project document");
    }
    // Project artifact routes resolve a slug, then a filename no other document shares, which is
    // what a dispatch:// reference means.
    const routed = await client
      .getProjectArtifact(owner.project, artifactReference)
      .catch((error: unknown) => {
        if (!dispatchAnswered(error, 404)) throw error;
        return undefined;
      });
    if (canonical && routed !== undefined) return { owner, artifact: routed };
    // A bare reference is matched as an issue's is, over the project's unlinked documents with the
    // route's answer among them: an id outranks another document's slug, and a slug another
    // document's filename. The unlinked-only collection keeps an issue-attached artifact of the
    // same name from becoming the document owner.
    const unlinked = await client.listProjectArtifacts(owner.project, true);
    const artifacts =
      routed === undefined
        ? unlinked
        : [routed, ...unlinked.filter((candidate) => candidate.id !== routed.id)];
    return {
      owner,
      artifact: artifactByReference(artifacts, artifactReference, owner, canonical),
    };
  }
  const issue = await client.getIssue(owner.issue);
  let artifact: Artifact | undefined;
  if (artifactReference === undefined || artifactReference === "spec") {
    artifact = issue.artifacts.find(
      (candidate) => candidate.primary || candidate.id === issue.primary_artifact_id
    );
  } else {
    artifact = artifactByReference(
      issue.artifacts,
      artifactReference,
      { kind: "issue", issue: issue.key },
      canonical
    );
  }
  if (!artifact) {
    throw new DocumentNotFoundError(
      documentReferenceProblem(artifactReference ?? "spec", issue.artifacts, {
        kind: "issue",
        issue: issue.key,
      })
    );
  }
  return { owner, issue, artifact };
}

/** How a call's documents resolve once its owner is known (`documentTargets`). */
export interface DocumentTargets {
  /** The call's owner, an issue reference resolved to the issue's key. */
  readonly owner: Owner | null;
  /** The owner, refused when the call names none. */
  documentOwner(): Owner;
  /** The document `reference` names under `owner`, read as the call's own reference reads it. */
  resolveDocument(owner: Owner, reference: string | undefined): Promise<ResolvedArtifact>;
}

/**
 * The owner a call's resolved arguments name, its issue reference resolved to the key, and how its
 * document references resolve: the document part of the call's dispatch:// reference is a slug by
 * definition (`canonical`), and a document argument the reference does not supply is a bare one, an
 * id, slug or filename. The executor and `readDocumentBlocks` resolve documents through it.
 */
export async function documentTargets(
  client: DispatchClient,
  resolution: OwnerResolution
): Promise<DocumentTargets> {
  const refDocument =
    resolution.ref?.kind === "artifact" ? resolution.ref.id : resolution.ref?.artifact;
  const owner: Owner | null =
    resolution.owner?.kind === "issue"
      ? { kind: "issue", issue: await resolveExistingIssue(client, resolution.owner.issue) }
      : resolution.owner;
  return {
    owner,
    documentOwner: () => {
      if (owner === null) throw new Error("issue or project is required");
      return owner;
    },
    resolveDocument: (documentOwner, reference) =>
      resolveArtifact(client, documentOwner, reference, {
        canonical: reference !== undefined && reference === refDocument,
      }),
  };
}

/** The id of the document an issue carries under `reference`: its artifact id, slug, or filename,
 * or `spec` for its primary document, resolved as the Dispatch tools resolve an issue's `artifact`
 * argument. A reference that names no document, or a filename two documents share, throws the
 * same hint the Dispatch tools give. */
export async function resolveIssueDocumentId(
  client: DispatchClient,
  issue: string,
  reference: string
): Promise<string> {
  return (await resolveArtifact(client, { kind: "issue", issue }, reference)).artifact.id;
}
const documentHintLimit = 8;

/** The refusal of a document reference: none matched, or, `ambiguous`, several share it as their
 * filename, which their slugs tell apart. An owner with more documents than the hint lists is
 * named with its count, the documents nearest the reference first (`documentsNearest`), and the
 * read that lists them all. */
function documentReferenceProblem(
  reference: string,
  documents: readonly (Pick<Artifact, "slug" | "name"> & { readonly kind?: Artifact["kind"] })[],
  owner: Owner,
  ambiguous = false
): string {
  const scope = owner.kind;
  const busy = !ambiguous && documents.length > documentHintLimit;
  const hints = (busy ? documentsNearest(reference, documents) : documents)
    .slice(0, documentHintLimit)
    .map((document) => `${document.slug} (${document.name})`);
  const list = hints.length === 0 ? "none" : hints.join(", ");
  if (busy) {
    const read =
      owner.kind === "issue"
        ? `dispatch read --issue ${owner.issue}`
        : `dispatch read --project ${owner.project}`;
    // `--artifact` names an uploaded picture or file as well as a document, so the hint counts
    // and ranks them too, and says so: Dispatch's own refusal counts documents alone.
    const counted = documents.every((document) => (document.kind ?? "doc") === "doc")
      ? "documents"
      : "documents and files";
    return `document "${reference}" not found by slug; this ${scope}'s ${documents.length} ${counted} include: ${list}; ${read} lists them all`;
  }
  return ambiguous
    ? `"${reference}" names ${documents.length} documents on this ${scope}; use a slug: ${list}`
    : `document "${reference}" not found by slug; this ${scope}'s documents: ${list}`;
}

/** `documents`, given oldest first as Dispatch lists them, ordered for a hint about `reference`:
 * those whose slug or filename is close to it (`documentCloseness`) first, nearest first, then
 * every other one, each group newest first. Dispatch's `documentsNearest`
 * (packages/envoy/internal/dispatch/api/artifacts.go) orders its own hint by the same rule. */
function documentsNearest<T extends Pick<Artifact, "slug" | "name">>(
  reference: string,
  documents: readonly T[]
): T[] {
  return documents
    .map((document, index) => ({
      document,
      index,
      ...documentCloseness(reference, document.slug, document.name),
    }))
    .sort((a, b) => {
      if (a.near !== b.near) return a.near ? -1 : 1;
      if (a.near && a.distance !== b.distance) return a.distance - b.distance;
      return b.index - a.index;
    })
    .map(({ document }) => document);
}

/** An owner's documents as a read lists them, one line each in the order Dispatch gives: slug,
 *  then filename and kind, the address `artifact` takes and the name a person sees. */
export function documentListLines(
  documents: readonly Pick<Artifact, "slug" | "name" | "kind">[]
): string[] {
  return documents.length === 0
    ? ["- none"]
    : documents.map((document) => `- ${document.slug} (${document.name}, ${document.kind})`);
}

/** A reference compared with a document's slug and filename, ASCII letter case ignored: the smaller
 * edit distance of the two, and whether the document is close to it - within a third of the
 * reference's length (at least 2 edits), or one holding the other where the one held is at least 4
 * characters long, whose distance is then exactly their difference in length. A spelling more than
 * `limit` edits away gives no distance (`boundedEditDistance` knows only that it is past `limit`),
 * so it never ties a document close through its other spelling. Characters are code points, as
 * Go's runes are, and only ASCII letters are folded, since JavaScript and Go lowercase other
 * letters differently (`İ`) and Dispatch's `documentCloseness` must rank alike. */
function documentCloseness(
  reference: string,
  slug: string,
  name: string
): { distance: number; near: boolean } {
  const wanted = [...reference.replace(/[A-Z]/g, (letter) => letter.toLowerCase())];
  const limit = Math.max(2, Math.floor(wanted.length / 3));
  let distance = Number.POSITIVE_INFINITY;
  for (const candidate of [slug, name]) {
    const label = [...candidate.replace(/[A-Z]/g, (letter) => letter.toLowerCase())];
    let edits = boundedEditDistance(wanted, label, limit);
    if (
      (wanted.length >= 4 && label.join("").includes(wanted.join(""))) ||
      (label.length >= 4 && wanted.join("").includes(label.join("")))
    ) {
      edits = Math.abs(label.length - wanted.length);
    } else if (edits > limit) {
      continue;
    }
    distance = Math.min(distance, edits);
  }
  return distance === Number.POSITIVE_INFINITY
    ? { distance: limit + 1, near: false }
    : { distance, near: true };
}

/** The Levenshtein distance between `a` and `b`, or `limit + 1` once it is known to be more. */
function boundedEditDistance(a: readonly string[], b: readonly string[], limit: number): number {
  if (Math.abs(a.length - b.length) > limit) return limit + 1;
  let previous = Array.from({ length: b.length + 1 }, (_, j) => j);
  let current = new Array<number>(b.length + 1).fill(0);
  for (let i = 1; i <= a.length; i++) {
    current[0] = i;
    let smallest = i;
    for (let j = 1; j <= b.length; j++) {
      const substitution = (previous[j - 1] as number) + (a[i - 1] === b[j - 1] ? 0 : 1);
      current[j] = Math.min(
        (previous[j] as number) + 1,
        (current[j - 1] as number) + 1,
        substitution
      );
      smallest = Math.min(smallest, current[j] as number);
    }
    if (smallest > limit) return limit + 1;
    [previous, current] = [current, previous];
  }
  return Math.min(previous[b.length] as number, limit + 1);
}
export function anchor(
  artifact: Artifact,
  args: Record<string, unknown>
): { artifact: string; quote: string; occurrence?: number } | undefined {
  const quote = optionalString(args, "quote");
  if (quote === undefined) return undefined;
  const occurrence = optionalNumber(args, "occurrence");
  return { artifact: artifact.id, quote, ...(occurrence === undefined ? {} : { occurrence }) };
}
/** One line describing a document's approval, or undefined for a draft nobody has asked about. An
 *  awaiting approval says whom its request waits on: the agent once a version moved it, the
 *  agent's own revision included, which sends that agent no event. */
export function approvalLine(artifact: Pick<Artifact, "approval">): string | undefined {
  const approval = artifact.approval;
  if (approval === undefined || approval.state === "draft") return undefined;
  switch (approval.state) {
    case "awaiting": {
      const turn = approval.waiting_on === undefined ? "" : `, waiting on ${approval.waiting_on}`;
      return `Approval: awaiting${turn} (requested by ${approval.requested_by?.id ?? "unknown"}, ask ${approval.ask_id ?? "?"})`;
    }
    case "approved":
      return `Approval: approved v${approval.version} by ${approval.by?.id ?? "unknown"}`;
    case "stale":
      return `Approval: approved v${approval.version} by ${approval.by?.id ?? "unknown"}, changed since (now v${approval.latest_version}) - the human's own answer to a decision block carries the approval when that answer is the whole change since v${approval.version}; for any other change, request approval again once the human has agreed to every point in this version, with a --summary naming what changed since v${approval.version}`;
    case "changes_requested":
      return `Approval: changes requested on v${approval.version} by ${approval.by?.id ?? "unknown"}: ${approval.reason ?? ""}`;
  }
}
/** A live document's blocks and the UTF-8 bytes of the markdown their ranges index. */
export interface LiveBlocks {
  readonly bytes: Uint8Array;
  readonly token: string | undefined;
  readonly blocks: ArtifactBlock[];
}

/**
 * A live document's blocks, read between two reads of its markdown, since Dispatch reports each
 * block's range as UTF-8 offsets into `/text`'s markdown; undefined when the token or the text
 * moved between the two reads, since the offsets would slice text they were not computed over.
 */
export async function liveBlocks(
  client: DispatchClient,
  artifact: Artifact
): Promise<LiveBlocks | undefined> {
  const before = await client.docRead(artifact.id);
  const blocks = await client.artifactBlocks(artifact.id);
  const after = await client.docRead(artifact.id);
  if (before.token !== after.token || before.markdown !== after.markdown) return undefined;
  return { bytes: new TextEncoder().encode(before.markdown), token: before.token, blocks };
}
/** A document as a Dispatch call names it: `dispatch doc-edit`'s owner and document arguments. */
export interface DocumentTarget {
  readonly issue?: string;
  readonly project?: string;
  readonly artifact?: string;
  readonly ref?: string;
}

/** What a target's owner is resolved against, as a call's arguments are: the checkout and
 *  environment the command runs in (`LEGION_ISSUE`, and the repository a bare issue number is
 *  on), and the Dispatch addresses a dashboard-URL `ref` may be on. */
export type DocumentTargetContext = Pick<
  DispatchCallCheckInput,
  "cwd" | "env" | "exec" | "serverUrls"
>;

/** One block of a document, and its markdown. */
export interface DocumentBlock {
  readonly id: string;
  readonly text: string;
}

/** A live document's blocks, and the document's own name and slug as Dispatch stores them. */
export interface DocumentBlocks {
  /**
   * The document's name. The reference that found it may be a slug, an id or `spec`, while
   * Dispatch finds the document an upload replaces by its exact name or its exact slug; a caller
   * reading for an upload compares the upload's name with both.
   */
  readonly name: string;
  /** The document's slug, the other reference an upload's name finds it by. */
  readonly slug: string;
  readonly blocks: DocumentBlock[];
}

/**
 * The blocks of the live document `target` names, in document order, each with its markdown: the
 * byte range Dispatch reports, sliced from the text it was computed over (`liveBlocks`), and the
 * document's own name and slug. The document is the one `dispatch doc-edit` edits for the same
 * arguments: its owner from the target, its ref or `LEGION_ISSUE`, and its document by id, slug,
 * filename, the ref's slug, or `spec` for an issue's primary document. A target naming no document
 * of its owner answers "not-found", which is what the first upload of a file reads. Everything
 * else throws: a refused target, a filename two documents answer to, a failed read, and a document
 * that changed between its two text reads. Every request carries the client's signal, so a stop
 * ends the read.
 */
export async function readDocumentBlocks(
  client: DispatchClient,
  target: DocumentTarget,
  context: DocumentTargetContext
): Promise<DocumentBlocks | "not-found"> {
  const tool = "dispatch_doc_edit";
  const args: ToolArguments = { ...target };
  const problems: string[] = [];
  const resolution = await resolveOwnerArguments(
    tool,
    args,
    context.cwd,
    context.env ?? process.env,
    context.exec ?? defaultExec,
    context.serverUrls,
    problems
  );
  problems.push(
    ...ownerConflictProblems(tool, args, resolution.ref, optionalString(resolution.args, "ref"))
  );
  if (problems.length > 0) throw new ToolInputError(tool, problems);
  const { documentOwner, resolveDocument } = await documentTargets(client, resolution);
  let resolved: ResolvedArtifact;
  try {
    resolved = await resolveDocument(documentOwner(), stringArg(resolution.args, "artifact"));
  } catch (error) {
    if (error instanceof DocumentNotFoundError) return "not-found";
    throw error;
  }
  const live = await liveBlocks(client, resolved.artifact);
  if (live === undefined) {
    throw new Error(
      `${resolved.artifact.name} changed while its blocks were read, so their byte ranges could slice the wrong text; nothing was read. Read it again.`
    );
  }
  const decoder = new TextDecoder();
  return {
    name: resolved.artifact.name,
    slug: resolved.artifact.slug,
    blocks: live.blocks.map((block) => ({
      id: block.id,
      text: decoder.decode(live.bytes.subarray(block.from, block.to)),
    })),
  };
}
async function resolveExistingIssue(
  client: DispatchClient,
  issueReference: string
): Promise<string> {
  try {
    return await client.resolveIssue(issueReference);
  } catch (error) {
    if (dispatchAnswered(error, 404)) {
      throw new Error(
        `no Dispatch issue is linked to ${issueReference}; create it first with ` +
          `${commandLine("dispatch_issue", { external: issueReference })} --project <key> --title <title>`
      );
    }
    throw error;
  }
}
