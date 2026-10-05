/**
 * The Dispatch tools' side of pictures: the files a call sends, checked and uploaded into the text
 * that shows them, and the pictures a read shows, each to a host session once. The rules both
 * sides and the hosts share (types, caps, the picture line and syntax) are `dispatch-pictures.ts`.
 */

import { basename, resolve as resolvePath } from "node:path";
import type {
  Actor,
  Artifact,
  ArtifactUploadResponse,
  AskRead,
  CreateArtifactInput,
  Event,
} from "@legion/contracts";
import { overCapMessage, SESSION_ID_PATTERN } from "@legion/contracts";
import type { DispatchClient } from "./dispatch-http";
import { parseDispatchRef } from "./dispatch-owner";
import {
  type DatedText,
  isPictureType,
  oversizePictureText,
  PICTURE_SHOWN_MAX_BYTES,
  PICTURE_SNIFF_BYTES,
  PICTURE_UPLOAD_MAX_BYTES,
  PICTURES_SHOWN_MAX,
  PICTURES_SHOWN_MAX_BYTES,
  pictureLine,
  sniffPictureType,
  type ToolImage,
  toolImage,
  withPictureLines,
} from "./dispatch-pictures";
import { messageFor } from "./errors";
import { ToolInputError } from "./tool-input-errors";

const PICTURE_TYPE_NAMES = "a PNG, JPEG, GIF or WebP";

/** A picture file `images` names, checked and ready to upload. */
export interface LocalPicture {
  /** As the call wrote it, which is how every refusal names it. */
  readonly path: string;
  /** Its file name: the upload's name and the picture's caption. */
  readonly name: string;
  /** The file, typed as its bytes say. */
  readonly file: Blob;
}

/**
 * The `images` files, in argument order. Each must be a PNG, JPEG, GIF or WebP by its bytes, never
 * its name, and at most `PICTURE_UPLOAD_MAX_BYTES`; each file that is not lands on `problems`
 * named by its path, in argument order, so the call is refused listing them all, with nothing
 * uploaded. The files are read at once.
 */
export async function localPictures(
  images: unknown,
  cwd: string,
  problems: string[]
): Promise<LocalPicture[]> {
  if (!Array.isArray(images)) return [];
  const checked = await Promise.all(images.map((path: unknown) => localPicture(path, cwd)));
  const pictures: LocalPicture[] = [];
  for (const outcome of checked) {
    if (typeof outcome === "string") problems.push(outcome);
    else if (outcome !== undefined) pictures.push(outcome);
  }
  return pictures;
}

/** One `images` entry as the picture it uploads, or the problem that refuses it; undefined for a
 *  value that is not a path, which the schema names. */
async function localPicture(
  path: unknown,
  cwd: string
): Promise<LocalPicture | string | undefined> {
  if (typeof path !== "string" || path === "") return undefined;
  const absolute = resolvePath(cwd, path);
  const file = Bun.file(absolute);
  let size: number;
  let head: Uint8Array;
  try {
    const stats = await file.stat();
    if (!stats.isFile()) return `images: ${path} is not a file`;
    size = stats.size;
    head = new Uint8Array(await file.slice(0, PICTURE_SNIFF_BYTES).arrayBuffer());
  } catch (error) {
    return `images: ${path} cannot be read: ${messageFor(error)}`;
  }
  if (size > PICTURE_UPLOAD_MAX_BYTES) {
    return `images: ${path} is ${size.toLocaleString("en-US")} bytes, over the ${PICTURE_UPLOAD_MAX_BYTES / 1024 / 1024} MiB one Dispatch upload takes`;
  }
  const type = sniffPictureType(head);
  if (type === undefined) {
    return `images: ${path} is not ${PICTURE_TYPE_NAMES} picture (judged by its bytes, not its name)`;
  }
  return { path, name: basename(absolute), file: Bun.file(absolute, { type }) };
}

/** The slug Dispatch gives a new upload named `name` that no other artifact of its owner holds
 *  (the server's `artifactSlug`): lowercased, its ASCII letters and digits kept and each other run
 *  one dash, the slug every reference grammar reads. */
function uploadSlug(name: string): string {
  const slug = name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");
  return slug === "" ? "artifact" : slug;
}

/** How a call's text is limited, and how a refusal names that text and what else shortens it. */
export interface PictureTextLimit {
  readonly cap: number;
  /** The text without its pictures: `body`, or `question plus ref`. */
  readonly text: string;
  /** The fix besides sending fewer pictures: `shorten the body`. */
  readonly shorten: string;
}

/**
 * `text` with `pictures` appended as the lines that show them (`withPictureLines`), each picture
 * uploaded first, in order, through `upload` to the owner `owner` names in an address (`KEY`,
 * `PROJECT`, or `agent/<session id>`). The cap is judged before anything is uploaded, on the
 * lines each picture gets as a new upload of its name (that name's slug, version 1). Dispatch can
 * address an upload longer than that - a later version of a name the owner already holds, or a
 * slug another file took first - so the text is judged again once the uploads named it, and
 * refused with nothing posted. An upload Dispatch refuses fails the call naming its path, with
 * nothing posted.
 */
export async function textWithPictures(
  tool: string,
  text: string,
  pictures: readonly LocalPicture[],
  limit: PictureTextLimit,
  owner: string,
  upload: (input: CreateArtifactInput) => Promise<ArtifactUploadResponse>,
  actor: Actor
): Promise<string> {
  if (pictures.length === 0) return text;
  const counted = `${limit.text} plus ${pictures.length} ${pictures.length === 1 ? "picture" : "pictures"}`;
  const fix = `${limit.shorten} or send fewer pictures`;
  const predicted = withPictureLines(
    text,
    pictures.map((picture) =>
      pictureLine(picture.name, `dispatch://${owner}/artifact/${uploadSlug(picture.name)}@v1`)
    )
  );
  if (predicted.length > limit.cap) {
    throw new ToolInputError(tool, [
      `${counted} ${overCapMessage(predicted.length, limit.cap)}; ${fix}`,
    ]);
  }
  const lines: string[] = [];
  const addresses: string[] = [];
  for (const picture of pictures) {
    let uploaded: ArtifactUploadResponse;
    try {
      uploaded = await upload({ name: picture.name, file: picture.file, actor });
    } catch (error) {
      const earlier =
        addresses.length === 0
          ? ""
          : `; the pictures before it are uploaded (${addresses.join(", ")})`;
      throw new Error(
        `images: ${picture.path} was not uploaded: ${messageFor(error)}. Nothing was posted${earlier}.`
      );
    }
    const address = `dispatch://${owner}/artifact/${uploaded.artifact.slug}@v${uploaded.version.number}`;
    addresses.push(address);
    lines.push(pictureLine(picture.name, address));
  }
  const posted = withPictureLines(text, lines);
  if (posted.length > limit.cap) {
    throw new Error(
      `${counted} ${overCapMessage(posted.length, limit.cap)} with the addresses Dispatch gave ` +
        `the uploads (${addresses.join(", ")}), so nothing was posted; ${fix}.`
    );
  }
  return posted;
}

/** An upload an agent's conversation on the Agents page owns, as its reference names it. */
export interface AgentArtifactRef {
  readonly session: string;
  readonly slug: string;
  readonly version?: number;
}

// The session id as `SESSION_ID_PATTERN` admits it, the rule the server and the dashboard read.
const AGENT_ARTIFACT_REF = new RegExp(
  `^dispatch://agent/(${SESSION_ID_PATTERN})/artifact/([^/@]+)(?:@v(\\d+))?$`,
  "u"
);

/**
 * `dispatch://agent/<session id>/artifact/<slug>[@vN]`. The session id is taken as written, never
 * percent-decoded, and is one `SESSION_ID_PATTERN` admits; the slug and the version read as an
 * issue artifact's do.
 */
export function parseAgentArtifactRef(ref: string): AgentArtifactRef | null {
  const match = ref.match(AGENT_ARTIFACT_REF);
  if (match === null) return null;
  const [, session, slug, version] = match;
  if (
    session === undefined ||
    slug === undefined ||
    (version !== undefined && Number(version) < 1)
  ) {
    return null;
  }
  return { session, slug, ...(version === undefined ? {} : { version: Number(version) }) };
}

/** The bodies of a thread's messages or comments, each dated, for the pictures a read shows. */
export function datedBodies(
  records: readonly { readonly body: string; readonly created_at: string }[]
): DatedText[] {
  return records.map((record) => ({ text: record.body, at: record.created_at }));
}

/** The texts an ask read shows, dated: its question, typed answer, resolution reason and replies. */
export function askTexts({ ask, replies }: AskRead): DatedText[] {
  return [
    { text: ask.question, at: ask.created_at },
    ...(ask.answer?.text ? [{ text: ask.answer.text, at: ask.answer.at }] : []),
    ...(ask.resolution === undefined
      ? []
      : [{ text: ask.resolution.reason, at: ask.resolution.at }]),
    ...datedBodies(replies),
  ];
}

/** The texts an event in a read's event list carries, dated by the event: an ask's question,
 *  typed answer and resolution reason, or a comment's or message's body. */
export function eventTexts(event: Event): DatedText[] {
  let texts: readonly (string | null | undefined)[];
  switch (event.type) {
    case "ask.opened":
    case "ask.anchor_refreshed":
    case "ask.edited":
    case "ask.handed_back":
    case "ask.answered":
    case "ask.resolved":
      texts = [
        event.payload.question,
        event.payload.answer?.text,
        event.payload.resolution?.reason,
      ];
      break;
    case "comment.created":
    case "comment.answered":
    case "comment.anchor_refreshed":
    case "comment.edited":
    case "comment.resolved":
    case "comment.reopened":
    case "suggestion.accepted":
    case "suggestion.rejected":
    case "message.created":
    case "message.answered":
      texts = [event.payload.body];
      break;
    default:
      texts = [];
  }
  return texts
    .filter((text): text is string => typeof text === "string" && text !== "")
    .map((text) => ({ text, at: event.created_at }));
}

/** Each picture a read shows, `PICTURES_SHOWN_MAX` and `PICTURES_SHOWN_MAX_BYTES` at most. */
export interface PicturesRead {
  /** `Pictures:`, then one line per address: the image it is shown as, or why it is not shown.
   *  Empty when there were no addresses. */
  readonly lines: readonly string[];
  /** The pictures shown, in the order `lines` numbers them. */
  readonly images: readonly ToolImage[];
}

/** The artifact a picture address names: an issue's, a project's or a conversation's upload, by
 *  its slug, and the version the address pins. */
interface PictureTarget {
  /** Dedupes concurrent reads of one artifact: `<owner>/<slug>`. */
  readonly key: string;
  readonly read: (client: DispatchClient) => Promise<Artifact>;
  readonly version: number;
}

/** Undefined for an address naming no versioned artifact. */
function pictureTarget(address: string): PictureTarget | undefined {
  const agent = parseAgentArtifactRef(address);
  if (agent !== null) {
    if (agent.version === undefined) return undefined;
    return {
      key: `agent/${agent.session}/${agent.slug}`,
      read: (client) => client.getAgentArtifact(agent.session, agent.slug),
      version: agent.version,
    };
  }
  const ref = parseDispatchRef(address);
  if (ref?.kind !== "artifact" || ref.version === undefined) return undefined;
  const { owner, id: slug } = ref;
  return owner.kind === "issue"
    ? {
        key: `${owner.issue}/${slug}`,
        read: (client) => client.getIssueArtifact(owner.issue, slug),
        version: ref.version,
      }
    : {
        key: `${owner.project}/${slug}`,
        read: (client) => client.getProjectArtifact(owner.project, slug),
        version: ref.version,
      };
}

// The addresses of the pictures each host session was shown, by session id.
const picturesShown = new Map<string, Set<string>>();

/**
 * The addresses of the pictures the host session `sessionId` was shown. A read or a delivery that
 * embeds one names it instead of sending its bytes again: every request a session makes carries
 * its whole history, so pictures sent again with each read add up until the provider refuses the
 * request (Anthropic's limit is 32 MB) and the session can make no progress. dispatch_doc_read
 * shows a picture whatever this holds, so a session whose history no longer has one, after
 * compaction, asks for it there.
 */
export function shownPictures(sessionId: string): Set<string> {
  let shown = picturesShown.get(sessionId);
  if (shown === undefined) {
    shown = new Set();
    picturesShown.set(sessionId, shown);
  }
  return shown;
}

/**
 * The pictures at `addresses` (newest first, each once) as a model is shown them: up to
 * `PICTURES_SHOWN_MAX` of them and `PICTURES_SHOWN_MAX_BYTES` together, each an image upload whose
 * bytes are a picture type a model takes, of at most `PICTURE_SHOWN_MAX_BYTES`. Every other address
 * is named with why it is not shown, so the reader opens it with dispatch_doc_read. A picture
 * Dispatch cannot serve is named with the failure; it never fails the read or the delivery around
 * it. Dispatch's stated size and type rule a picture out before its bytes are fetched. An address
 * in `shown`, the pictures this session was already shown, is named as shown earlier, read
 * nothing and counts toward neither limit; each picture shown here joins `shown`.
 */
export async function readPictures(
  client: DispatchClient,
  addresses: readonly string[],
  shown?: Set<string>
): Promise<PicturesRead> {
  if (addresses.length === 0) return { lines: [], images: [] };
  const artifacts = new Map<string, Promise<Artifact>>();
  const images: ToolImage[] = [];
  const lines = ["Pictures:"];
  let shownBytes = 0;
  const overBudget = `past this read's ${PICTURES_SHOWN_MAX_BYTES / 1024 / 1024} MiB of pictures; dispatch_doc_read shows it`;
  for (const address of addresses) {
    const skip = (reason: string) => lines.push(`- not shown: ${address} (${reason})`);
    if (shown?.has(address)) {
      skip("shown earlier this session; dispatch_doc_read shows it again");
      continue;
    }
    if (images.length === PICTURES_SHOWN_MAX) {
      skip(`past this read's ${PICTURES_SHOWN_MAX} pictures; dispatch_doc_read shows it`);
      continue;
    }
    const target = pictureTarget(address);
    if (target === undefined) {
      skip("names no versioned Dispatch artifact");
      continue;
    }
    try {
      let read = artifacts.get(target.key);
      if (read === undefined) {
        read = target.read(client);
        artifacts.set(target.key, read);
      }
      const artifact = await read;
      const version = artifact.versions.find((candidate) => candidate.number === target.version);
      if (version === undefined) {
        skip(`${artifact.name} has no version ${target.version}`);
        continue;
      }
      if (artifact.kind !== "image") {
        skip(`${artifact.name} is a ${artifact.kind}, not a picture`);
        continue;
      }
      const mime = version.mime?.split(";")[0]?.trim();
      if (mime !== undefined && !isPictureType(mime)) {
        skip(`${artifact.name} is ${mime}, not ${PICTURE_TYPE_NAMES} a model is shown`);
        continue;
      }
      if (version.size !== undefined && version.size > PICTURE_SHOWN_MAX_BYTES) {
        skip(oversizePictureText(artifact.name, version.size));
        continue;
      }
      if (version.size !== undefined && shownBytes + version.size > PICTURES_SHOWN_MAX_BYTES) {
        skip(overBudget);
        continue;
      }
      const file = await client.fileVersion(artifact.id, target.version);
      const image = toolImage(file.bytes);
      if (image === undefined) {
        skip(
          sniffPictureType(file.bytes) === undefined
            ? `${artifact.name} is not ${PICTURE_TYPE_NAMES} a model is shown`
            : oversizePictureText(artifact.name, file.bytes.length)
        );
        continue;
      }
      if (shownBytes + file.bytes.length > PICTURES_SHOWN_MAX_BYTES) {
        skip(overBudget);
        continue;
      }
      shownBytes += file.bytes.length;
      images.push(image);
      shown?.add(address);
      lines.push(
        `- image ${images.length}: ${address} (${artifact.name}, ${image.mimeType}, ${file.bytes.length.toLocaleString("en-US")} bytes)`
      );
    } catch (error) {
      skip(`unavailable: ${messageFor(error)}`);
    }
  }
  return { lines, images };
}
