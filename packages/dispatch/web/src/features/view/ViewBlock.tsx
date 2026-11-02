import { useQueries, useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";

import { ApiError, api } from "../../api/client";
import type {
  ArtifactGrant,
  DocumentReferenceSummary,
  IssueReferenceSummary,
} from "../../api/types";
import { borderDefault, dangerText, linkText, textMutedOnSurface } from "../../theme/classes";
import { documentSummaryQuery, issueSummaryQuery } from "../refs/reference-target";
import { buildReferencePath } from "../refs/routes";
import { registerViewFrame } from "./frame-budget";
import { readViewAttributes, type ViewFile } from "./view-attributes";
import type { ViewBlockSlot } from "./view-slots";

/** The frame's height before the page reports one, and the bounds a reported height is held to
 *  (dispatch://INFRA-682, "The frame"). */
const DEFAULT_FRAME_HEIGHT = 400;
const MIN_FRAME_HEIGHT = 100;
const MAX_FRAME_HEIGHT = 2_000;

/** Dispatch sends a frame at most one `dispatch-view:data` in this time, with the newest version
 *  of each file, so a file that changes many times a second cannot flood a phone. */
const SEND_INTERVAL_MS = 2_000;

/** A page that sends no `dispatch-view:ready` this long after its load shows the line instead. */
const READY_DEADLINE_MS = 10_000;

/** The most bytes one block's granted data may hold together; over it Dispatch sends no data. */
const MAX_DATA_BYTES = 16 * 1024 * 1024;

/** The most heights one frame applies. A page that posts a height forever would otherwise hold
 *  an animation frame for as long as the reader stays on the page. */
const MAX_HEIGHT_CHANGES = 200;

/** Why the page route refused, as the block's line says it. */
const PAGE_REFUSALS: Record<number, string> = {
  404: "this page file is not found",
  413: "this page is larger than 5 MiB",
  415: "this file is not a page file",
};

/** What a summary read of one file has answered so far. */
interface SummaryResult {
  readonly data: DocumentReferenceSummary | IssueReferenceSummary | undefined;
  readonly error: unknown;
  readonly isError: boolean;
}

/** One file of a block as its summary read answers: the version the block reads and that
 *  version's size, or why the file cannot show. Neither `cause` nor `id` means the read has not
 *  answered yet. */
interface FileState {
  readonly cause?: string;
  readonly id?: string;
  readonly name: string;
  readonly ref: string;
  readonly size: number;
  readonly version?: number;
}

/** One entry of a `dispatch-view:data` message, and the file it was built from. */
interface DataEntry {
  readonly contentType?: string;
  readonly error?: string;
  readonly id?: string;
  readonly name?: string;
  readonly ref: string;
  readonly status?: number;
  readonly text?: string;
  readonly version?: number;
}

/** The same-origin address of one version's bytes, which the block reads with the reader's own
 *  session, as every artifact read is made. `artifactVersionUrl`
 *  (`features/artifacts/ArtifactHeader.tsx`) writes the same string, and importing it here
 *  would close the cycle `ViewBlock → ArtifactHeader → ApprovalChip → MarkdownBody →
 *  useRenderedMarkdown → ViewBlock` that `lint/suspicious/noImportCycles` refuses. */
function versionAddress(id: string, version: number): string {
  return `/api/v1/artifacts/${encodeURIComponent(id)}/versions/${version}`;
}

/** What a read that failed says: the status Dispatch answered, or that it could not be reached
 *  at all. One wording, for the page and for a data file alike. */
function answerCause(status: number | undefined, what: string): string {
  const answer =
    status === undefined ? "Dispatch could not be reached" : `Dispatch answered ${status}`;
  return `${answer} for this ${what}`;
}

/** No read has failed: the lines a block starts with, and the map it keeps while every read
 *  answers its text. */
const noReadCauses: ReadonlyMap<string, string> = new Map();

/** The file one summary read names, resolved to what the block needs of it. An issue's summary
 *  carries every document of that issue, so one read serves every file of one issue. */
function resolveFile(file: ViewFile, result: SummaryResult): FileState {
  const base = { name: file.slug, ref: file.reference, size: 0 };
  const summary = result.data;
  if (summary === undefined) {
    // Either the read has not answered yet, or it failed and the line says how.
    if (!result.isError) {
      return base;
    }
    const status = result.error instanceof ApiError ? result.error.status : undefined;
    return { ...base, cause: answerCause(status, "file") };
  }
  const record =
    "documents" in summary
      ? summary.documents.find((document) => document.slug === file.slug)
      : summary;
  if (record === undefined) {
    return { ...base, cause: "this file is not found" };
  }
  const named = { ...base, name: record.name };
  if (record.kind === "doc") {
    return { ...named, cause: "a document is not a file a view reads" };
  }
  const latest = record.latest_version;
  const pinned = file.version;
  if (latest === null || (pinned !== undefined && pinned > latest)) {
    return {
      ...named,
      cause:
        pinned === undefined ? "this file has no version" : `this file has no version ${pinned}`,
    };
  }
  return { ...named, id: record.id, size: record.size ?? 0, version: pinned ?? latest };
}

/**
 * The grant that covers one data file, and the version it gives the page: the newest version
 * under a grant without a version, the pinned version under a grant that pins one, and for a
 * reference that pins `@vN`, only a grant without a version or a grant pinned to the same N
 * (dispatch://INFRA-682, "Data grants").
 */
function coveringGrant(
  grants: readonly ArtifactGrant[],
  state: FileState,
  pinned: number | undefined
): { id: string; version: number } | undefined {
  const newest = state.version;
  if (state.id === undefined || newest === undefined) {
    return undefined;
  }
  let best: { id: string; version: number } | undefined;
  for (const grant of grants) {
    if (grant.artifact_id !== state.id) {
      continue;
    }
    if (pinned !== undefined) {
      if (grant.version === null || grant.version === pinned) {
        return { id: grant.id, version: pinned };
      }
      continue;
    }
    if (grant.version === null) {
      return { id: grant.id, version: newest };
    }
    if (best === undefined || grant.version > best.version) {
      best = { id: grant.id, version: grant.version };
    }
  }
  return best;
}

/** One line a reader sees in place of, or beside, the frame: the reference it names, why, and
 *  whether it links to the panel where a person grants the file. */
interface ViewLine {
  readonly cause: string;
  readonly panel?: boolean;
  readonly ref: string;
}

/** One quiet line beside the frame: that the block has no caption, that it waits for a frame of
 *  the budget, or that the page has no data yet. */
function note(words: string): ReactNode {
  return <p className={textMutedOnSurface}>{words}</p>;
}

/**
 * A `view` block: the sandboxed frame that shows its page, and the lines a reader sees when
 * Dispatch cannot show it. It renders into the block's own frame slot, which no repaint takes
 * out of the document, so the frame and its page keep running through a caption edit, a streamed
 * turn and another session's edit of the body around it.
 *
 * One effect owns the one frame that is running and everything about it: the element, how many
 * times it loaded, whether the page answered `ready`, when it last sent data and the version of
 * each file it sent. Those live as that effect's own variables, so the page that moved away is
 * never mistaken for the frame that replaced it, and the effect's cleanup ends them. The block
 * holds beside them what the `HEAD` of the page route answered and whether the page moved away.
 * The budget (`frame-budget.ts`) decides only whether this block is among the four frames that
 * run, and the block reads nothing at all until it says yes. A change of the grants that cover
 * the named files starts a new frame, so a page that lost a grant loses the data it held.
 */
export function ViewBlock({ slot }: { slot: ViewBlockSlot }): ReactNode {
  const { caption, data, host, page } = slot;
  const attributes = useMemo(() => readViewAttributes(page, data), [data, page]);
  const named = attributes.kind === "files" ? [attributes.page, ...attributes.data] : [];
  const [running, setRunning] = useState(false);
  const [movedAway, setMovedAway] = useState(false);
  const [silent, setSilent] = useState(false);
  const [head, setHead] = useState<{ address: string; cause: string | undefined }>();
  const [sent, setSent] = useState(false);
  /** Why the bytes of a data file did not arrive, by reference, as the newest send found it.
   *  A later read of the same file that answers its text takes its line away again. */
  const [readCauses, setReadCauses] = useState(noReadCauses);
  const holder = useRef<HTMLDivElement>(null);
  /** The frame that is running now, and the call that sends it the newest data: the two things
   *  the effect that owns a frame lends to the renders and effects around it. */
  const element = useRef<HTMLIFrameElement>(undefined);
  const scheduleSend = useRef<() => void>(undefined);
  const texts = useRef(new Map<string, DataEntry>());

  const watched = attributes.kind === "files";
  useEffect(() => {
    if (watched) {
      return registerViewFrame(host, setRunning);
    }
    return;
  }, [host, watched]);

  const summaries = useQueries({
    queries: named.map((file) => ({
      ...("issue" in file.owner
        ? issueSummaryQuery(file.owner.issue)
        : documentSummaryQuery(file.owner.project, file.slug)),
      enabled: running,
    })),
  });
  // `useQueries` answers one result for each query, so every named file has one of its own.
  const states = named.map((file, index) => resolveFile(file, summaries[index]));
  const pageState = states[0];
  const pageId = pageState?.id;

  const grants = useQuery({
    enabled: running && pageId !== undefined,
    queryFn: () => api.listArtifactGrants(pageId ?? ""),
    queryKey: ["artifact-grants", pageId],
    staleTime: Number.POSITIVE_INFINITY,
  });

  // Every line this block shows, in the order a reader reads them: what the attributes or the
  // page file cannot answer, then one line per data file, then what the frame itself answered.
  const lines: ViewLine[] = [];
  if (attributes.kind === "problem") {
    lines.push({ cause: attributes.cause, ref: attributes.reference });
  }
  const pageCause = pageState?.cause;
  if (pageCause !== undefined) {
    lines.push({ cause: pageCause, ref: page });
  }

  // One pass over the data files: what goes to the page, what the reader is told, how many bytes
  // the grants cover, which grants cover them, and which versions the page has already had.
  const rows = grants.data;
  const entries: DataEntry[] = [];
  let cover = "";
  let sendSignature = "";
  let bytes = 0;
  for (let index = 1; index < states.length; index += 1) {
    const state = states[index];
    const grant = rows === undefined ? undefined : coveringGrant(rows, state, named[index].version);
    let entry: DataEntry;
    if (state.cause !== undefined) {
      lines.push({ cause: state.cause, ref: state.ref });
      entry = { error: "unreadable", ref: state.ref };
    } else if (grant === undefined) {
      if (rows !== undefined) {
        lines.push({ cause: "not granted to this page", panel: true, ref: state.ref });
      }
      entry = { error: "not-granted", ref: state.ref };
    } else {
      bytes += state.size;
      entry = { id: state.id, name: state.name, ref: state.ref, version: grant.version };
      const failure = readCauses.get(state.ref);
      if (failure !== undefined) {
        lines.push({ cause: failure, ref: state.ref });
      }
    }
    entries.push(entry);
    cover += grant === undefined ? " none" : ` ${grant.id}`;
    sendSignature += ` ${entry.ref}@${entry.error ?? entry.version}`;
  }
  const overBudget = rows !== undefined && bytes > MAX_DATA_BYTES;
  const entriesRef = useRef(entries);
  entriesRef.current = entries;
  /** The cap as the frame's own effect reads it: the page's `dispatch-view:ready` schedules the
   *  first send, and it arrives whenever the page loads, which can be after the grants read
   *  answered. Without this the cap would hold only for the sends the render schedules. */
  const overBudgetRef = useRef(overBudget);
  overBudgetRef.current = overBudget;

  const pageVersion = pageState?.version;
  const pageAddress =
    pageId === undefined || pageVersion === undefined
      ? undefined
      : `${versionAddress(pageId, pageVersion)}/page`;
  const answered = head !== undefined && head.address === pageAddress ? head : undefined;
  const refusal = answered?.cause;
  const canRun = running && answered !== undefined && refusal === undefined && !movedAway;
  const frameTitle = caption === "" ? page : caption;

  for (const cause of [
    refusal,
    movedAway ? "the page moved away from its address" : undefined,
    silent ? `this page sent no ready message in ${READY_DEADLINE_MS / 1_000} seconds` : undefined,
  ]) {
    if (cause !== undefined) {
      lines.push({ cause, ref: page });
    }
  }
  if (overBudget) {
    lines.push({
      cause: `the data files together are larger than ${MAX_DATA_BYTES / 1024 ** 2} MiB, so Dispatch sends no data`,
      ref: "data",
    });
  }

  // One HEAD before the frame, so a refusal of the page route is a line rather than an empty
  // frame. A page over 5 MiB, a file that is no page and a version that is not there answer it.
  useEffect(() => {
    // The answer cannot have changed: `pageAddress` carries the version number, and a version's
    // size and type are fixed, so a block that scrolls back into the budget sends no second HEAD.
    if (!running || pageAddress === undefined || head?.address === pageAddress) {
      return;
    }
    let cancelled = false;
    void fetch(pageAddress, { credentials: "same-origin", method: "HEAD" }).then(
      (response) => {
        const cause = response.ok
          ? undefined
          : (PAGE_REFUSALS[response.status] ?? answerCause(response.status, "page"));
        if (!cancelled) setHead({ address: pageAddress, cause });
      },
      () => {
        if (!cancelled) {
          setHead({ address: pageAddress, cause: answerCause(undefined, "page") });
        }
      }
    );
    return () => {
      cancelled = true;
    };
  }, [head, pageAddress, running]);

  // A change of which grants cover the named files is a new frame, so a page that lost a grant
  // loses the data it holds; a new version of a granted file changes no grant and no frame.
  // biome-ignore lint/correctness/useExhaustiveDependencies: `cover` is the set of grants, which this effect starts a frame for; its body reads the files through refs.
  useEffect(() => {
    const parent = holder.current;
    if (!canRun || parent === null || pageAddress === undefined) {
      return;
    }
    setSilent(false);
    setSent(false);
    const frame = document.createElement("iframe");
    frame.setAttribute("sandbox", "allow-scripts");
    frame.setAttribute("referrerpolicy", "no-referrer");
    frame.className = "block w-full border-0";
    frame.style.height = `${DEFAULT_FRAME_HEIGHT}px`;
    // The address is set before the frame is inserted, so the frame loads once and this block
    // counts that one load.
    frame.src = pageAddress;
    element.current = frame;

    // Everything this one frame holds, and nothing of the frame before it.
    let stopped = false;
    let loads = 0;
    let ready = false;
    let sentAt = 0;
    let heightChanges = 0;
    let pendingHeight = DEFAULT_FRAME_HEIGHT;
    // A timer and an animation frame are always above zero, so zero is "none running".
    let heightFrame = 0;
    let readyTimer = 0;
    let sendTimer = 0;

    /** One data file's bytes as the page gets them, or why they did not arrive. The three
     *  causes are different: an answer that is not `ok` carries its status, a `fetch` that
     *  rejects carries no status, and bytes that are not UTF-8 text are `not-text`. */
    const readVersion = async (
      entry: DataEntry,
      id: string,
      version: number
    ): Promise<DataEntry> => {
      const { name, ref } = entry;
      const response = await fetch(versionAddress(id, version), {
        credentials: "same-origin",
      }).catch(() => undefined);
      if (response === undefined || !response.ok) {
        return { error: "read-failed", ref, status: response?.status };
      }
      const bytes = await response.arrayBuffer().catch(() => undefined);
      if (bytes === undefined) {
        return { error: "read-failed", ref };
      }
      try {
        return {
          contentType: response.headers.get("Content-Type") ?? "application/octet-stream",
          name,
          ref,
          text: new TextDecoder("utf-8", { fatal: true }).decode(bytes),
          version,
        };
      } catch {
        // The decode is the only throw here, so these bytes are not UTF-8 text. This one file
        // goes to the page as `not-text`, and every other file still goes with it.
        return { error: "not-text", ref };
      }
    };

    /** Sends the newest version of every file the grants cover. The bytes are read as the
     *  message goes out, so ten uploads inside one wait cost one read of the newest version.
     *  The reads one message needs are started together, since they are independent reads of
     *  this same origin: the page waits for the longest of them rather than for their sum. One
     *  version is read once however many entries name it, as a `data` attribute may name one
     *  file twice. */
    const send = async (): Promise<void> => {
      const held = texts.current;
      // The entries as they stand now, walked twice: the reads are started over the first walk,
      // and the second walk, over the answers the message carries in that same order, tells the
      // reader what did not arrive and holds what did.
      const current = entriesRef.current;
      const started = new Map<string, Promise<DataEntry>>();
      const files = await Promise.all(
        current.map((entry) => {
          const { id, version } = entry;
          if (id === undefined || version === undefined) {
            // An entry no grant covers, or one no read could name, goes as it stands.
            return entry;
          }
          const read = held.get(id);
          if (read?.version === version) {
            return read;
          }
          const key = `${id}@${version}`;
          const reading = started.get(key) ?? readVersion(entry, id, version);
          started.set(key, reading);
          return reading;
        })
      );
      const failures = new Map<string, string>();
      for (const [index, read] of files.entries()) {
        const { id, version } = current[index];
        if (id === undefined || version === undefined) {
          continue;
        }
        if (read.text === undefined) {
          // The reader gets a line for this file, and the page gets the entry beside the
          // files that did answer their text.
          failures.set(
            read.ref,
            read.error === "not-text"
              ? "this file is not UTF-8 text"
              : answerCause(read.status, "file")
          );
          continue;
        }
        held.set(id, read);
      }
      if (stopped || loads > 1) {
        return;
      }
      // The same set of lines is the same map, which is no new render.
      setReadCauses((old) =>
        old.size === failures.size && [...failures].every(([ref, cause]) => old.get(ref) === cause)
          ? old
          : failures
      );
      sentAt = Date.now();
      // The page's origin is opaque, so there is no origin to name but any.
      frame.contentWindow?.postMessage({ files, type: "dispatch-view:data" }, "*");
      setSent(true);
    };

    const schedule = (): void => {
      if (!ready || sendTimer !== 0 || overBudgetRef.current) {
        return;
      }
      sendTimer = window.setTimeout(
        () => {
          sendTimer = 0;
          void send();
        },
        Math.max(0, SEND_INTERVAL_MS - (Date.now() - sentAt))
      );
    };
    scheduleSend.current = schedule;

    // The block owns this element alone, so the one `load` handler is its own property.
    frame.onload = () => {
      loads += 1;
      if (loads === 1) {
        readyTimer = window.setTimeout(() => setSilent(true), READY_DEADLINE_MS);
        return;
      }
      // The page moved its own frame to another address. Nothing more goes to it, and the block
      // starts no new frame until the reader loads the Dispatch page again.
      setMovedAway(true);
    };
    const onMessage = (event: MessageEvent) => {
      const message: unknown = event.data;
      if (
        event.source !== frame.contentWindow ||
        loads > 1 ||
        typeof message !== "object" ||
        message === null ||
        !("type" in message)
      ) {
        return;
      }
      if (message.type === "dispatch-view:ready") {
        if (ready) {
          return;
        }
        ready = true;
        window.clearTimeout(readyTimer);
        schedule();
        return;
      }
      const height =
        message.type === "dispatch-view:height" && "height" in message ? message.height : undefined;
      if (typeof height !== "number" || !Number.isFinite(height)) {
        return;
      }
      pendingHeight = Math.min(MAX_FRAME_HEIGHT, Math.max(MIN_FRAME_HEIGHT, Math.round(height)));
      if (heightFrame === 0 && heightChanges < MAX_HEIGHT_CHANGES) {
        heightFrame = window.requestAnimationFrame(() => {
          heightFrame = 0;
          heightChanges += 1;
          frame.style.height = `${pendingHeight}px`;
        });
      }
    };
    window.addEventListener("message", onMessage);
    parent.appendChild(frame);
    return () => {
      stopped = true;
      scheduleSend.current = undefined;
      element.current = undefined;
      window.removeEventListener("message", onMessage);
      window.clearTimeout(readyTimer);
      window.clearTimeout(sendTimer);
      window.cancelAnimationFrame(heightFrame);
      frame.remove();
    };
  }, [canRun, cover, pageAddress]);

  // The frame's title follows the caption, so another session's caption edit renames the frame
  // and starts no new one. It runs on every render, because a new frame is a new element to name
  // and the effect above has already made it by the time this one runs.
  useEffect(() => {
    if (element.current !== undefined) {
      element.current.title = frameTitle;
    }
  });

  // biome-ignore lint/correctness/useExhaustiveDependencies: a new signature is a new set of versions to send, which is what this effect schedules; the send itself reads the newest through a ref.
  useEffect(() => {
    if (!overBudget) {
      scheduleSend.current?.();
    }
  }, [overBudget, sendSignature]);

  // The panel where a person grants a data file sits on the page file's own view.
  const panel = attributes.kind === "files" ? buildReferencePath(attributes.page.route) : undefined;

  return (
    <div className="not-prose mt-2 text-sm">
      {caption === "" ? note(`${page} — this view has no caption`) : null}
      {lines.map((line) => (
        <p className={dangerText} key={`${line.ref} ${line.cause}`}>
          {line.ref} — {line.cause}
          {line.panel === true && panel !== undefined ? (
            <>
              {" "}
              <a className={linkText} href={panel}>
                Open the page file
              </a>
            </>
          ) : null}
        </p>
      ))}
      {watched && !running ? note("waiting") : null}
      {canRun && !sent ? note("loading") : null}
      <div className={`overflow-hidden rounded-lg border ${borderDefault}`} ref={holder} />
    </div>
  );
}
