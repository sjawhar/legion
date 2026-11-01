import { useQueries, useQuery } from "@tanstack/react-query";
import {
  type ReactNode,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import { createPortal } from "react-dom";

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

/** One `view` block's frame slot and what the block says about itself: the `page` and `data`
 *  attributes as the author wrote them, and the caption, which is the frame's title. */
export interface ViewBlockSlot {
  readonly caption: string;
  readonly data: string;
  readonly host: HTMLElement;
  readonly key: string;
  readonly page: string;
}

/** Every frame slot the engine drew under `root` (`renderViewBlockShell`), with the attributes
 *  and caption its `ViewBlock` renders from. The caption is the block's content hole, the slot's
 *  own previous sibling, so a caption edit reaches the block without touching the frame. */
export function collectViewSlots(root: HTMLElement): ViewBlockSlot[] {
  const slots: ViewBlockSlot[] = [];
  for (const host of root.querySelectorAll<HTMLElement>("[data-dispatch-view-frame]")) {
    const page = host.getAttribute("data-dispatch-view-page") ?? "";
    const hole = host.previousElementSibling;
    slots.push({
      caption:
        (hole?.matches("[data-proof-block-content]") === true ? hole.textContent : "")?.trim() ??
        "",
      data: host.getAttribute("data-dispatch-view-data") ?? "",
      host,
      key: `view:${page}:${slots.length}`,
      page,
    });
  }
  return slots;
}

/**
 * The frame slots of the document editors (`features/doc/view-block.ts`). A node view is not
 * React, so it registers its slot here and `EditorViewBlocks`, which the live editor and the
 * version view render, portals a `ViewBlock` into each. A body's slots are collected from its
 * own element instead (`collectViewSlots`), since a body is painted whole on each repaint.
 */
const editorSlots = new Map<string, ViewBlockSlot>();
let editorSnapshot: readonly ViewBlockSlot[] = [];
const editorListeners = new Set<() => void>();

/** Adds the node view's slot, or writes the one already under its key: a caption edit or a new
 *  `data` attribute reaches the mounted `ViewBlock` without a new frame. */
export function setEditorViewSlot(slot: ViewBlockSlot): void {
  editorSlots.set(slot.key, slot);
  editorSnapshot = [...editorSlots.values()];
  for (const listener of [...editorListeners]) {
    listener();
  }
}

export function removeEditorViewSlot(key: string): void {
  if (editorSlots.delete(key)) {
    editorSnapshot = [...editorSlots.values()];
    for (const listener of [...editorListeners]) {
      listener();
    }
  }
}

/** A `ViewBlock` in every frame slot a document editor's node views have registered. The live
 *  editor (`ProofDocument`) and the version view render it once each. */
export function EditorViewBlocks(): ReactNode {
  const slots = useSyncExternalStore(
    (listener: () => void) => {
      editorListeners.add(listener);
      return () => {
        editorListeners.delete(listener);
      };
    },
    () => editorSnapshot
  );
  return slots.map((slot) =>
    createPortal(
      <ViewBlock caption={slot.caption} data={slot.data} host={slot.host} page={slot.page} />,
      slot.host,
      slot.key
    )
  );
}

/** What a summary read of one file has answered so far. */
interface SummaryResult {
  readonly data: DocumentReferenceSummary | IssueReferenceSummary | undefined;
  readonly error: unknown;
  readonly isError: boolean;
  readonly isPending: boolean;
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
  readonly text?: string;
  readonly version?: number;
}

/** The frame that is running now. Every count belongs to that one element, so the page that
 *  moved away is never mistaken for the frame that replaced it. */
interface FrameRun {
  readonly frame: HTMLIFrameElement;
  heightChanges: number;
  heightFrame: number | undefined;
  loads: number;
  pendingHeight: number;
  ready: boolean;
  readyTimer: number | undefined;
  sendTimer: number | undefined;
  sentAt: number;
}

/** The same-origin address of one version's bytes, which the block reads with the reader's own
 *  session, as every artifact read is made. */
function versionAddress(id: string, version: number): string {
  return `/api/v1/artifacts/${encodeURIComponent(id)}/versions/${version}`;
}

/** What a read that failed says: the status Dispatch answered, or that it could not be reached
 *  at all. One wording, for the page and for a data file alike. */
function answerCause(status: number | undefined, what: string): string {
  return status === undefined
    ? `Dispatch could not be reached for this ${what}`
    : `Dispatch answered ${status} for this ${what}`;
}

/** The file one summary read names, resolved to what the block needs of it. An issue's summary
 *  carries every document of that issue, so one read serves every file of one issue. */
function resolveFile(file: ViewFile, result: SummaryResult): FileState {
  const base = { name: file.slug, ref: file.reference, size: 0 };
  if (result.isPending) {
    return base;
  }
  if (result.isError) {
    const status = result.error instanceof ApiError ? result.error.status : undefined;
    return { ...base, cause: answerCause(status, "file") };
  }
  const summary = result.data;
  const record =
    summary === undefined
      ? undefined
      : "documents" in summary
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
  let best: { id: string; version: number } | undefined;
  for (const grant of grants) {
    if (grant.artifact_id !== state.id || state.version === undefined) {
      continue;
    }
    if (pinned !== undefined) {
      if (grant.version === null || grant.version === pinned) {
        return { id: grant.id, version: pinned };
      }
      continue;
    }
    if (grant.version === null) {
      return { id: grant.id, version: state.version };
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

/**
 * A `view` block: the sandboxed frame that shows its page, and the lines a reader sees when
 * Dispatch cannot show it. It renders into the block's own frame slot, which no repaint takes
 * out of the document, so the frame and its page keep running through a caption edit, a streamed
 * turn and another session's edit of the body around it.
 *
 * The block holds everything about the one frame it runs: the element, how many times it loaded,
 * whether the page answered `ready`, when it last sent data, the versions it sent, what the
 * `HEAD` of the page route answered, and whether the page moved away from its address. The
 * budget (`frame-budget.ts`) decides only whether this block is among the four frames that run,
 * and the block reads nothing at all until it says yes. A change of the grants that cover the
 * named files starts a new frame, so a page that lost a grant loses the data it held.
 */
export function ViewBlock({
  caption,
  data,
  host,
  page,
}: {
  caption: string;
  data: string;
  host: HTMLElement;
  page: string;
}): ReactNode {
  const attributes = useMemo(() => readViewAttributes(page, data), [data, page]);
  const named = attributes.kind === "files" ? [attributes.page, ...attributes.data] : [];
  const [running, setRunning] = useState(false);
  const [movedAway, setMovedAway] = useState(false);
  const [silent, setSilent] = useState(false);
  const [generation, setGeneration] = useState(0);
  const [head, setHead] = useState<{ address: string; cause: string | undefined }>();
  const [sent, setSent] = useState(false);
  const holder = useRef<HTMLDivElement>(null);
  const runRef = useRef<FrameRun>(undefined);
  const texts = useRef(new Map<string, DataEntry>());

  const watched = attributes.kind === "files";
  useEffect(() => {
    if (watched) {
      return registerViewFrame(host, setRunning);
    }
    return;
  }, [host, watched]);

  const summaries = useQueries({
    queries: named.map((file) => {
      const owner = file.owner;
      return "issue" in owner
        ? { ...issueSummaryQuery(owner.issue), enabled: running }
        : { ...documentSummaryQuery(owner.project, file.slug), enabled: running };
    }),
  });
  const pending: SummaryResult = {
    data: undefined,
    error: undefined,
    isError: false,
    isPending: true,
  };
  const states = named.map((file, index) => resolveFile(file, summaries[index] ?? pending));
  const pageState = states[0];

  const grants = useQuery({
    enabled: running && pageState?.id !== undefined,
    queryFn: () => api.listArtifactGrants(pageState?.id ?? ""),
    queryKey: ["artifact-grants", pageState?.id],
    staleTime: Number.POSITIVE_INFINITY,
  });

  // One pass over the data files: what goes to the page, what the reader is told, how many bytes
  // the grants cover, and which grants cover them.
  const rows = grants.data;
  const entries: DataEntry[] = [];
  const lines: ViewLine[] = [];
  const cover: string[] = [];
  let bytes = 0;
  for (const [index, state] of states.slice(1).entries()) {
    const grant =
      rows === undefined ? undefined : coveringGrant(rows, state, named[index + 1]?.version);
    cover.push(grant?.id ?? "none");
    if (state.cause !== undefined) {
      lines.push({ cause: state.cause, ref: state.ref });
      entries.push({ error: "unreadable", ref: state.ref });
      continue;
    }
    if (grant === undefined) {
      if (rows !== undefined) {
        lines.push({ cause: "not granted to this page", panel: true, ref: state.ref });
      }
      entries.push({ error: "not-granted", ref: state.ref });
      continue;
    }
    bytes += state.size;
    entries.push({ id: state.id, name: state.name, ref: state.ref, version: grant.version });
  }
  const overBudget = rows !== undefined && bytes > MAX_DATA_BYTES;
  const entriesRef = useRef(entries);
  entriesRef.current = entries;
  const sendSignature = entries
    .map((entry) => `${entry.ref}@${entry.error ?? entry.version}`)
    .join(" ");
  const coverSignature = rows === undefined ? undefined : cover.join(" ");

  const pageAddress =
    pageState?.id === undefined || pageState.version === undefined
      ? undefined
      : `${versionAddress(pageState.id, pageState.version)}/page`;
  const answered = head !== undefined && head.address === pageAddress ? head : undefined;
  const canRun = running && answered?.cause === undefined && answered !== undefined && !movedAway;
  const frameTitle = caption === "" ? page : caption;
  const titleRef = useRef(frameTitle);
  titleRef.current = frameTitle;

  /** Sends the newest version of every file the grants cover. The bytes are read as the message
   *  goes out, so ten uploads inside one wait cost one read of the newest version. */
  const send = useCallback(async (run: FrameRun): Promise<void> => {
    const held = texts.current;
    const files: DataEntry[] = [];
    for (const entry of entriesRef.current) {
      const { id, version } = entry;
      if (id === undefined || version === undefined) {
        files.push(entry);
        continue;
      }
      let read = held.get(id);
      if (read?.version !== version) {
        read = await fetch(versionAddress(id, version), { credentials: "same-origin" }).then(
          async (response) =>
            response.ok
              ? {
                  contentType: response.headers.get("Content-Type") ?? "application/octet-stream",
                  name: entry.name,
                  ref: entry.ref,
                  text: new TextDecoder("utf-8", { fatal: true }).decode(
                    await response.arrayBuffer()
                  ),
                  version,
                }
              : { error: "unreadable", ref: entry.ref },
          () => ({ error: "not-text", ref: entry.ref })
        );
        if (read.text === undefined) {
          files.push(read);
          continue;
        }
        held.set(id, read);
      }
      files.push(read);
    }
    if (runRef.current !== run || run.loads > 1) {
      return;
    }
    run.sentAt = Date.now();
    // The page's origin is opaque, so there is no origin to name but any.
    run.frame.contentWindow?.postMessage({ files, type: "dispatch-view:data" }, "*");
    setSent(true);
  }, []);

  const schedule = useCallback(() => {
    const run = runRef.current;
    if (run === undefined || !run.ready || run.sendTimer !== undefined) {
      return;
    }
    run.sendTimer = window.setTimeout(
      () => {
        run.sendTimer = undefined;
        void send(run);
      },
      Math.max(0, SEND_INTERVAL_MS - (Date.now() - run.sentAt))
    );
  }, [send]);
  const scheduleRef = useRef(schedule);
  scheduleRef.current = schedule;

  // One HEAD before the frame, so a refusal of the page route is a line rather than an empty
  // frame. A page over 5 MiB, a file that is no page and a version that is not there answer it.
  useEffect(() => {
    if (!running || pageAddress === undefined) {
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
  }, [pageAddress, running]);

  // A change of which grants cover the named files starts a new frame, so a page that lost a
  // grant loses the data it holds. A new version of a granted file changes no grant and no frame.
  const lastCover = useRef(coverSignature);
  useEffect(() => {
    if (coverSignature !== undefined && coverSignature !== lastCover.current) {
      lastCover.current = coverSignature;
      setGeneration((value) => value + 1);
    }
  }, [coverSignature]);

  // biome-ignore lint/correctness/useExhaustiveDependencies: a new generation is a new frame, which is what this effect makes; its body reads no generation.
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
    frame.title = titleRef.current;
    frame.className = "block w-full border-0";
    frame.style.height = `${DEFAULT_FRAME_HEIGHT}px`;
    // The address is set before the frame is inserted, so the frame loads once and this block
    // counts that one load.
    frame.src = pageAddress;
    const run: FrameRun = {
      frame,
      heightChanges: 0,
      heightFrame: undefined,
      loads: 0,
      pendingHeight: DEFAULT_FRAME_HEIGHT,
      ready: false,
      readyTimer: undefined,
      sendTimer: undefined,
      sentAt: 0,
    };
    runRef.current = run;
    const onLoad = () => {
      run.loads += 1;
      if (run.loads === 1) {
        run.readyTimer = window.setTimeout(() => setSilent(true), READY_DEADLINE_MS);
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
        run.loads > 1 ||
        typeof message !== "object" ||
        message === null ||
        !("type" in message)
      ) {
        return;
      }
      if (message.type === "dispatch-view:ready") {
        if (run.ready) {
          return;
        }
        run.ready = true;
        window.clearTimeout(run.readyTimer);
        run.readyTimer = undefined;
        scheduleRef.current();
        return;
      }
      const height =
        message.type === "dispatch-view:height" && "height" in message ? message.height : undefined;
      if (typeof height !== "number" || !Number.isFinite(height)) {
        return;
      }
      run.pendingHeight = Math.min(
        MAX_FRAME_HEIGHT,
        Math.max(MIN_FRAME_HEIGHT, Math.round(height))
      );
      if (run.heightFrame === undefined && run.heightChanges < MAX_HEIGHT_CHANGES) {
        run.heightFrame = window.requestAnimationFrame(() => {
          run.heightFrame = undefined;
          run.heightChanges += 1;
          run.frame.style.height = `${run.pendingHeight}px`;
        });
      }
    };
    frame.addEventListener("load", onLoad);
    window.addEventListener("message", onMessage);
    parent.appendChild(frame);
    return () => {
      window.removeEventListener("message", onMessage);
      frame.removeEventListener("load", onLoad);
      window.clearTimeout(run.readyTimer);
      window.clearTimeout(run.sendTimer);
      if (run.heightFrame !== undefined) {
        window.cancelAnimationFrame(run.heightFrame);
      }
      frame.remove();
      if (runRef.current === run) {
        runRef.current = undefined;
      }
    };
  }, [canRun, generation, pageAddress]);

  useEffect(() => {
    const run = runRef.current;
    if (run !== undefined) {
      run.frame.title = frameTitle;
    }
  }, [frameTitle]);

  // biome-ignore lint/correctness/useExhaustiveDependencies: a new signature is a new set of versions to send, which is what this effect schedules; the send itself reads the newest through a ref.
  useEffect(() => {
    if (!overBudget) {
      scheduleRef.current();
    }
  }, [overBudget, sendSignature]);

  if (attributes.kind === "problem") {
    lines.unshift({ cause: attributes.problem.cause, ref: attributes.problem.reference });
  }
  if (pageState?.cause !== undefined) {
    lines.unshift({ cause: pageState.cause, ref: page });
  }
  if (answered?.cause !== undefined) {
    lines.push({ cause: answered.cause, ref: page });
  }
  if (movedAway) {
    lines.push({ cause: "the page moved away from its address", ref: page });
  }
  if (silent) {
    lines.push({ cause: "this page sent no ready message in 10 seconds", ref: page });
  }
  if (overBudget) {
    lines.push({
      cause: "the data files together are larger than 16 MiB, so Dispatch sends no data",
      ref: "data",
    });
  }
  // The panel where a person grants a data file sits on the page file's own view.
  const pageRoute = named[0]?.route;
  const panel = pageRoute === undefined ? undefined : buildReferencePath(pageRoute);

  return (
    <div className="not-prose mt-2 text-sm">
      {caption === "" ? (
        <p className={textMutedOnSurface}>{page} — this view has no caption</p>
      ) : null}
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
      {watched && !running ? <p className={textMutedOnSurface}>waiting</p> : null}
      {canRun && !sent ? <p className={textMutedOnSurface}>loading</p> : null}
      <div className={`overflow-hidden rounded-lg border ${borderDefault}`} ref={holder} />
    </div>
  );
}
