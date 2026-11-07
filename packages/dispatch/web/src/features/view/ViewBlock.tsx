import { useQueries, useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";

import { ApiError, artifactVersionUrl } from "../../api/client";
import { borderDefault, dangerText, linkText, textMutedOnSurface } from "../../theme/classes";
import {
  artifactGrantsQuery,
  documentSummaryQuery,
  issueSummaryQuery,
} from "../refs/reference-target";
import { buildReferencePath } from "../refs/routes";
import { PAGE_DEADLINE_MS, registerViewFrame, setViewFrameLoading } from "./frame-budget";
import { readViewAttributes } from "./view-attributes";
import {
  answerCause,
  coveringGrant,
  type DataEntry,
  planViewData,
  resolveFile,
  type ViewLine,
} from "./view-data-plan";
import type { ViewBlockSlot } from "./view-slots";

/** The frame's height before the page reports one, and the bounds a reported height is held to
 *  (dispatch://INFRA-682, "The frame"). */
const DEFAULT_FRAME_HEIGHT = 400;
const MIN_FRAME_HEIGHT = 100;
const MAX_FRAME_HEIGHT = 2_000;

/** Dispatch sends a frame at most one `dispatch-view:data` in this time, with the newest version
 *  of each file, so a file that changes many times a second cannot flood a phone. */
const SEND_INTERVAL_MS = 2_000;

/** The most heights one frame applies. A page that posts a height forever would otherwise hold
 *  an animation frame for as long as the reader stays on the page. */
const MAX_HEIGHT_CHANGES = 200;

/** Why the page route refused, as the block's line says it. */
const PAGE_REFUSALS: Record<number, string> = {
  404: "this page file is not found",
  413: "this page is larger than 5 MiB",
  415: "this file is not a page file",
};

/** One version's size in bytes, as the `HEAD` of its address answers `Content-Length`. A
 *  summary carries the latest version's size alone, so this is what a block weighs against the
 *  16 MiB whenever the version it sends is an older one, whether the reference or the grant
 *  pinned it (dispatch://INFRA-682, R6). The server answers the `HEAD` from the row's length,
 *  reading none of the bytes. */
async function versionSize(id: string, version: number): Promise<number> {
  const response = await fetch(artifactVersionUrl(id, version), {
    credentials: "same-origin",
    method: "HEAD",
  });
  const length = Number(response.headers.get("Content-Length"));
  if (!response.ok || !Number.isFinite(length)) {
    throw new ApiError(response.status, { error: "this version's size cannot be read" });
  }
  return length;
}

/** No read has failed: the lines a block starts with, and the map it keeps while every read
 *  answers its text. */
const noReadCauses: ReadonlyMap<string, string> = new Map();

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
 * run, and the block reads nothing at all until it says yes; the block tells it back when its
 * frame is loading its page (`setViewFrameLoading`), which is the one time the budget may not take
 * the slot away. A change of the grants that cover the named files starts a new frame, so a
 * page that lost a grant loses the data it held.
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

  // The grants stay as read until an event or a write of the panel's own says otherwise
  // (`staleTime`), since a block that scrolls back into the budget must not pay for a read it
  // already has, nor rebuild its frame on the answer.
  const grants = useQuery({
    ...artifactGrantsQuery(pageId, Number.POSITIVE_INFINITY),
    enabled: running && pageId !== undefined,
  });

  const rows = grants.data;
  /** The grant that covers each data file and the version it gives the page: the version the
   *  block weighs and sends, which is the version whose size is read below. Index 0 is the page
   *  file, which no grant covers. `planViewData` reads the same rule for what it sends. */
  const covers = states.map((state, index) =>
    index === 0 || rows === undefined ? undefined : coveringGrant(rows, state)
  );

  /** Each data file's size where the summary does not carry it, which is a version a pin — the
   *  reference's or the grant's — holds back from the latest: one `HEAD` of that version's
   *  address. The key is the version's own, so one `HEAD` serves every block that names it, and
   *  a file the block sends the latest version of asks nothing. The page file's own size is
   *  weighed against nothing, so index 0 asks nothing either. */
  const sizes = useQueries({
    queries: states.map((state, index) => {
      const version = covers[index]?.version;
      return {
        enabled: running && version !== undefined && version !== state.latest,
        queryFn: () => versionSize(state.id ?? "", version ?? 0),
        queryKey: ["artifact-version-size", state.id, version],
        staleTime: Number.POSITIVE_INFINITY,
      };
    }),
  });

  // Every line this block shows, in the order a reader reads them: what the attributes or the
  // page file cannot answer, then the data files' own, then what the frame itself answered.
  const lines: ViewLine[] = [];
  if (attributes.kind === "problem") {
    lines.push({ cause: attributes.cause, ref: attributes.reference });
  }
  const pageCause = pageState?.cause;
  if (pageCause !== undefined) {
    lines.push({ cause: pageCause, ref: page });
  }

  // What the page is given and what the reader is told about each data file: one pure pass over
  // what the reads have answered (`view-data-plan.ts`), which is where the grant rule and the
  // 16 MiB cap live and where their cases are tested.
  const {
    cover,
    entries,
    holdData,
    lines: dataLines,
    sendSignature,
  } = planViewData(states, rows, sizes, readCauses);
  lines.push(...dataLines);
  const entriesRef = useRef(entries);
  entriesRef.current = entries;
  /** The hold as the frame's own effect reads it: the page's `dispatch-view:ready` schedules
   *  the first send, and it arrives whenever the page loads, which can be after the grants read
   *  answered. Without this the cap would hold only for the sends the render schedules. */
  const holdDataRef = useRef(holdData);
  holdDataRef.current = holdData;

  const pageVersion = pageState?.version;
  const pageAddress =
    pageId === undefined || pageVersion === undefined
      ? undefined
      : `${artifactVersionUrl(pageId, pageVersion)}/page`;
  const answered = head !== undefined && head.address === pageAddress ? head : undefined;
  const refusal = answered?.cause;
  /** The grants read has answered, either way. The frame's own effect is keyed on the set of
   *  grants (`cover`), so a frame started before that read answered is rebuilt the moment it
   *  does: the engine aborts the navigation the first frame had started, and the block pays for
   *  a page it never showed (dispatch://INFRA-682). A read that failed answers too - the block
   *  then runs its page with no data rather than not at all. */
  const grantsAnswered = rows !== undefined || grants.isError;
  const canRun =
    running && answered !== undefined && refusal === undefined && !movedAway && grantsAnswered;
  const frameTitle = caption === "" ? page : caption;

  for (const cause of [
    refusal,
    movedAway ? "the page moved away from its address" : undefined,
    silent ? `this page sent no ready message in ${PAGE_DEADLINE_MS / 1_000} seconds` : undefined,
  ]) {
    if (cause !== undefined) {
      lines.push({ cause, ref: page });
    }
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
    /** The channel every data message of this frame goes over: one end of a `MessageChannel`
     *  whose other end was transferred to the page with the answer to its first `ready`. A
     *  transferred port belongs to the document that received it, so a page that moves its
     *  frame away takes the port out of the world with its own document and the replacement
     *  has nothing to receive on (dispatch://INFRA-682, R5). */
    let port: MessagePort | undefined;
    /** Whether a send is reading its files now, and whether anything changed while it read.
     *  One send runs at a time, so a read of an older version can never be posted after a
     *  newer one, and every change made during a send is one follow-up send when it ends
     *  (dispatch://INFRA-682, R3). */
    let sending = false;
    let sendAgain = false;

    /** One data file's bytes as the page gets them, or why they did not arrive. The three
     *  causes are different: an answer that is not `ok` carries its status, a `fetch` that
     *  rejects carries no status, and bytes that are not UTF-8 text are `not-text`. */
    const readVersion = async (
      entry: DataEntry,
      id: string,
      version: number
    ): Promise<DataEntry> => {
      const { name, ref } = entry;
      const response = await fetch(artifactVersionUrl(id, version), {
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

    /** Sends the newest version of every file the grants cover, over the page's own port. The
     *  bytes are read as the message goes out, so ten uploads inside one wait cost one read of
     *  the newest version. The reads one message needs are started together, since they are
     *  independent reads of this same origin: the page waits for the longest of them rather
     *  than for their sum. One version is read once however many entries name it, as a `data`
     *  attribute may name one file twice. Only `schedule` below calls this, and only with no
     *  other send running. */
    const send = async (): Promise<void> => {
      // The cap is read here as well as where this send was scheduled: a version that arrives
      // inside the wait can be the one that takes the block's data past 16 MiB, and then none
      // of it goes (dispatch://INFRA-682, R6).
      if (holdDataRef.current) {
        return;
      }
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
      // Over the port alone. The frame's window is no channel: by the time a message is posted
      // the page may have moved that window to another address, and the page's opaque origin
      // leaves `"*"` as the only target origin to name, so nothing in the send would hold it
      // back (dispatch://INFRA-682, R5).
      port?.postMessage({ files, type: "dispatch-view:data" });
      setSent(true);
    };

    /** Puts the next send on the clock: at most one in `SEND_INTERVAL_MS`, one at a time, and
     *  one follow-up for every change made while a send was reading. */
    const schedule = (): void => {
      if (stopped || !ready || holdDataRef.current) {
        return;
      }
      if (sending) {
        // A send is reading now. Nothing starts beside it: its own end schedules the one
        // follow-up, which reads the versions that stand then, so an older read is never
        // posted after a newer one and ten changes inside one send are one more send.
        sendAgain = true;
        return;
      }
      if (sendTimer !== 0) {
        return;
      }
      sendTimer = window.setTimeout(
        () => {
          sendTimer = 0;
          sending = true;
          void send().finally(() => {
            sending = false;
            if (sendAgain) {
              sendAgain = false;
              schedule();
            }
          });
        },
        Math.max(0, SEND_INTERVAL_MS - (Date.now() - sentAt))
      );
    };
    scheduleSend.current = schedule;

    // The block owns this element alone, so the one `load` handler is its own property.
    frame.onload = () => {
      if (stopped) {
        // The cleanup below took this frame out of the document, which aborts the navigation it
        // was making, and the engine reports that abort as a `load` of its own. Nothing of this
        // frame is the block's any more: a ready timer started here would belong to no frame on
        // the page and would say the page sent no ready message, over a frame that has
        // (dispatch://INFRA-682).
        return;
      }
      loads += 1;
      if (loads === 1) {
        // The page is in: the budget may take this slot away now without aborting anything.
        setViewFrameLoading(host, false);
        readyTimer = window.setTimeout(() => setSilent(true), PAGE_DEADLINE_MS);
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
        // The one channel this document gets, with the first answer to its first `ready`. Only
        // the first is answered, so a document that replaced this one and posts a `ready` of
        // its own is given no port (dispatch://INFRA-682, R5). The page's origin is opaque, so
        // there is no origin to name but any.
        const channel = new MessageChannel();
        port = channel.port1;
        frame.contentWindow?.postMessage({ type: "dispatch-view:port" }, "*", [channel.port2]);
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
    // The navigation starts with the insertion, so the budget holds this slot from here until
    // the page has loaded: a slot taken away in between would abort it.
    setViewFrameLoading(host, true);
    parent.appendChild(frame);
    return () => {
      stopped = true;
      setViewFrameLoading(host, false);
      scheduleSend.current = undefined;
      element.current = undefined;
      window.removeEventListener("message", onMessage);
      window.clearTimeout(readyTimer);
      window.clearTimeout(sendTimer);
      window.cancelAnimationFrame(heightFrame);
      port?.close();
      frame.remove();
    };
  }, [canRun, cover, host, pageAddress]);

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
    if (!holdData) {
      scheduleSend.current?.();
    }
  }, [holdData, sendSignature]);

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
