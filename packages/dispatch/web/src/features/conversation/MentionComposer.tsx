import { type MutationKey, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type ClipboardEvent,
  type DragEvent,
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
  type SyntheticEvent,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { ApiError, api, apiErrorMessage } from "../../api/client";
import { agentMessagesQuery } from "../../api/queries";
import type { Agent, AskOption, AskUrgency } from "../../api/types";
import { Chip } from "../../components/Chip";
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import { TruncatedText } from "../../components/TruncatedText";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { pastDeadlineKey } from "../../hooks/useSending";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  badgeMed,
  borderDefault,
  borderStrong,
  calloutInfoBg,
  calloutInfoBorder,
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  checkboxAccent,
  controlHoverBorder,
  dangerHoverBorder,
  dangerText,
  dismissButtonText,
  focusRing,
  hoverToDangerText,
  inputClasses,
  linkHoverText,
  linkText,
  primaryButtonBg,
  quoteAccentBorder,
  quoteBodyText,
  referencePillBorder,
  secondaryButtonText,
  surfaceBg,
  surfaceMutedHoverBg,
  surfaceRecessedBg,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { uploadErrorMessage, uploadFile } from "../artifacts/ArtifactUpload";
import { ASK_URGENCIES_ASCENDING, URGENCY_LABELS } from "../inbox/ask-urgency";
import { ReferencePicker } from "../refs/ReferencePicker";
import { buildDispatchReference, composerReferences } from "../refs/routes";
import type {
  AcceptedMention,
  ComposerAnchor,
  ComposerKind,
  ComposerOwner,
  ReplyTarget,
} from "./composer-model";
import { MODE_LABELS } from "./delivery";
import { ReplyQuote, replyQuoteText } from "./ReplyQuote";
import {
  DELIVERY_COMMANDS,
  type DeliveryPlan,
  deliveryPlan,
  mentionText,
  SEND_DEADLINE_MS,
  SendDeadlineError,
  type SentRequest,
  sendWithinDeadline,
  survivingMentions,
} from "./send-request";
import { useAgents } from "./useAgents";

interface TextEditRange {
  readonly end: number;
  readonly start: number;
}

/** A draft as the composer holds it: the text, the mentions accepted in it, and, when it is a
 *  suggestion, its replacement. Mentions are offsets into that exact text. */
interface Draft {
  readonly body: string;
  readonly mentions: readonly AcceptedMention[];
  readonly replacement?: string;
}

interface MentionOption {
  readonly capabilities: readonly string[];
  readonly detail: string;
  readonly target: string;
  readonly title: string;
}

interface AskOptionDraft {
  description: string;
  id: number;
  label: string;
}

let nextAskOptionId = 0;

function emptyAskOption(): AskOptionDraft {
  nextAskOptionId += 1;
  return { description: "", id: nextAskOptionId, label: "" };
}

function submittedAskOptions(options: readonly AskOptionDraft[]): AskOption[] {
  const submitted: AskOption[] = [];
  for (const option of options) {
    const label = option.label.trim();
    const description = option.description.trim();
    if (label !== "") {
      submitted.push(description === "" ? { label } : { description, label });
    }
  }
  return submitted;
}

function roleOptions(agents: readonly Agent[]): MentionOption[] {
  const byRole = new Map<string, Agent>();
  for (const agent of agents) {
    for (const role of agent.roles) byRole.set(role, agent);
  }
  return [...byRole.entries()]
    .sort(([left], [right]) => {
      const leftController = left.includes("controller");
      const rightController = right.includes("controller");
      return leftController === rightController
        ? left.localeCompare(right)
        : leftController
          ? -1
          : 1;
    })
    .map(([role, agent]) => ({
      capabilities: agent.capabilities,
      detail: `${agent.title} · ${agent.dir} · ${new Date(agent.last_seen).toLocaleString()}`,
      target: `role:${role}`,
      title: role,
    }));
}

export function mentionOptions(agents: readonly Agent[]): MentionOption[] {
  return [
    ...roleOptions(agents),
    ...[...agents]
      .sort(
        (left, right) => right.last_seen - left.last_seen || left.title.localeCompare(right.title)
      )
      .map((agent) => ({
        capabilities: agent.capabilities,
        detail: `${agent.dir} · ${new Date(agent.last_seen).toLocaleString()}`,
        target: `session:${agent.session_id}`,
        title: agent.title === "" ? agent.session_id : agent.title,
      })),
  ];
}

function appendReference(body: string, reference: string): string {
  return `${body}${body.length === 0 || /\s$/.test(body) ? "" : " "}${reference}`;
}

/** Reconcile accepted mentions against the actual textarea edit range. */
export function reconcileMentions(
  before: string,
  after: string,
  mentions: readonly AcceptedMention[],
  edit?: TextEditRange
): AcceptedMention[] {
  if (edit !== undefined) {
    const delta = after.length - before.length;
    return mentions.flatMap((mention) => {
      if (mention.end <= edit.start) return [mention];
      if (mention.start >= edit.end) {
        return [{ ...mention, end: mention.end + delta, start: mention.start + delta }];
      }
      return [];
    });
  }
  const delta = after.length - before.length;
  if (delta < 0) {
    const deleted = -delta;
    const possibleStarts = Array.from({ length: after.length + 1 }, (_, start) => start).filter(
      (start) =>
        before.slice(0, start) === after.slice(0, start) &&
        before.slice(start + deleted) === after.slice(start)
    );
    return mentions.flatMap((mention) => {
      if (possibleStarts.some((start) => mention.start < start + deleted && mention.end > start)) {
        return [];
      }
      const shifts = new Set(possibleStarts.map((start) => (start < mention.start ? delta : 0)));
      if (shifts.size !== 1) return [];
      const shift = shifts.values().next().value;
      if (shift === undefined) return [];
      return [{ ...mention, end: mention.end + shift, start: mention.start + shift }];
    });
  }
  let prefix = 0;
  const common = Math.min(before.length, after.length);
  while (prefix < common && before[prefix] === after[prefix]) prefix += 1;
  return mentions.flatMap((mention) => {
    if (mention.end <= prefix) return [mention];
    if (mention.start >= prefix) {
      return [{ ...mention, end: mention.end + delta, start: mention.start + delta }];
    }
    return [];
  });
}

function mentionQuery(
  body: string,
  selectionStart: number | null
): { query: string; start: number } | undefined {
  const caret = selectionStart ?? body.length;
  const before = body.slice(0, caret);
  const at = before.lastIndexOf("@");
  if (at < 0 || /\s/.test(before.slice(at + 1))) return undefined;
  return { query: before.slice(at + 1), start: at };
}

function mentionDisplay(title: string, target: string): string {
  const trimmed = title.trim();
  if (trimmed !== "" && !/^(?:session|role):/i.test(trimmed) && !/[@\r\n]/.test(trimmed)) {
    return trimmed;
  }
  return target.replace(":", " ");
}

/** The target a message in this channel already reaches: mentioning it says nothing. */
function ownTarget(owner: ComposerOwner): string | null {
  return owner.kind === "session" ? `session:${owner.sessionId}` : null;
}

/**
 * What a draft becomes under a channel's seed: the reader's prose with the records still in step
 * with it, and the channel's own mentions seeded in front of whatever is not already there. A
 * mount applies it to no draft at all; a host that changes the seed - an Agents row's issue pick,
 * or a reply that moves the message to another channel - has it applied to the live draft in
 * place, so the draft never parts from the composer, or from a send it has in flight.
 *
 * Three rules keep the text and the accepted records saying the same thing, because only the
 * records reach the wire (`survivingMentions` at send) while only the text reaches the reader:
 *
 * - A record counts only while its span is untouched (`survivingMentions`). A span the reader
 *   edited is their prose: the record goes, the text stays.
 * - The channel's own mention belongs to the channel. Entering one that seeds none - a direct
 *   message, which already reaches its session - takes the untouched record for that session out
 *   of the text with its separating space. Every other surviving record stays, so a trip back
 *   restores it.
 * - Seeding is by target, not by position: a mention this channel owes is added only when no
 *   surviving record already names it, wherever in the body that record sits.
 */
function seededDraft(
  seedMentions: readonly { readonly target: string; readonly title: string }[],
  draft: Draft | undefined,
  owner: ComposerOwner
): { body: string; mentions: AcceptedMention[] } {
  let body = draft?.body ?? "";
  let records = survivingMentions(body, draft?.mentions ?? []);
  const own = ownTarget(owner);
  const addressed = own === null ? undefined : records.find((record) => record.target === own);
  if (addressed !== undefined) {
    const cut =
      body.slice(addressed.end, addressed.end + 1) === " " ? addressed.end + 1 : addressed.end;
    const width = cut - addressed.start;
    body = body.slice(0, addressed.start) + body.slice(cut);
    records = records
      .filter((record) => record !== addressed)
      .map((record) =>
        record.start >= cut
          ? { ...record, end: record.end - width, start: record.start - width }
          : record
      );
  }
  const owed = seedMentions.filter(
    (mention) => !records.some((record) => record.target === mention.target)
  );
  let offset = 0;
  const seeded = owed.map((mention) => {
    const text = mentionDisplay(mention.title, mention.target);
    const start = offset;
    offset += mentionText(text).length;
    const end = offset;
    offset += 1;
    return { end, start, target: mention.target, text };
  });
  const prefix = owed
    .map((mention) => mentionText(mentionDisplay(mention.title, mention.target)))
    .join(" ");
  const shift = prefix === "" ? 0 : prefix.length + 1;
  return {
    body: [prefix, body].filter((part) => part !== "").join(" "),
    mentions: [
      ...seeded,
      ...records.map((record) => ({
        ...record,
        end: record.end + shift,
        start: record.start + shift,
      })),
    ],
  };
}

export function hasUnsavedInput(
  body: string,
  replacement: string,
  options: readonly Pick<AskOptionDraft, "description" | "label">[] = []
): boolean {
  return (
    body.trim().length > 0 ||
    replacement.trim().length > 0 ||
    options.some((option) => option.label.trim().length > 0 || option.description.trim().length > 0)
  );
}

/** Whether Send takes the draft now: nothing refuses it (`draftRefusal`, the one rule for what an
 *  empty draft is) and no save or upload it waits on is in flight. */
export function canSubmitComposer(
  refusal: string | undefined,
  isSaving: boolean,
  pendingUploads: number
): boolean {
  return refusal === undefined && !isSaving && pendingUploads === 0;
}

/**
 * Why Send refuses a draft it is not busy with, or undefined when it would take it: an empty
 * draft, and a delivery command with nothing after it - `/btw ` alone, where the box holds text
 * and Send is still dead. A save or an upload in flight names itself on the button instead.
 */
function draftRefusal(
  kind: ComposerKind,
  body: string,
  replacement: string,
  outbound: DeliveryPlan
): string | undefined {
  if (kind === "suggestion") {
    return replacement.trim() === "" ? "Type the replacement text first." : undefined;
  }
  if (body.trim() === "")
    return kind === "ask" ? "Type the question first." : "Type a message first.";
  if (
    kind === "comment" &&
    outbound.delivery !== undefined &&
    outbound.delivery !== "steer" &&
    outbound.body.trim() === ""
  ) {
    return `Type the message after ${DELIVERY_COMMANDS[outbound.delivery]}.`;
  }
  return undefined;
}

interface MentionComposerProps {
  readonly agents?: readonly Agent[];
  readonly anchor?: ComposerAnchor;
  readonly autoFocus?: boolean;
  /** The owner takes no more sends: its issue is closed. The composer stays mounted, so a send
   *  still out and its refusal keep their draft: it shows itself only while it holds one of them,
   *  with Send refused and Discard in place of Retry, and otherwise renders nothing, its draft
   *  kept for a reopen. */
  readonly closed?: boolean;
  readonly docked?: boolean;
  readonly edit?: { readonly body: string; readonly id: string };
  /** The channel's own mentions: seeded at mount, again by every reset (a send, Discard), and
   *  into the live draft whenever they change (`seededDraft`). */
  readonly seedMentions?: readonly { readonly target: string; readonly title: string }[];
  readonly inline?: boolean;
  readonly kind?: ComposerKind;
  /** Names the send, so a host can hold its own controls - a Reply, an issue picker - while it is
   *  out (`useSending`), from Send's own task on. That decides only what the reader sees: the
   *  request is frozen when Send starts (`SentRequest`), whatever those controls do. */
  readonly mutationKey?: MutationKey;
  readonly onCancelReply?: () => void;
  readonly onClose: () => void;
  readonly onSent: () => void;
  readonly owner: ComposerOwner;
  readonly replyTo?: ReplyTarget | null;
  readonly saveEdit?: (id: string, body: string) => Promise<unknown>;
  /** Shows the Comment / Suggest / Ask switch and hands each pick to the host, which answers
   *  through `kind` - a selected document mark is the only surface where the kind can change, and
   *  its mark has to change with it (the margin retypes it) - or returns, in the reader's words,
   *  why it refused, which the composer shows under the switch. */
  readonly onKindChange?: (kind: ComposerKind) => string | undefined;
}

export function MentionComposer({
  agents: suppliedAgents,
  anchor,
  autoFocus = false,
  closed = false,
  docked = false,
  edit,
  seedMentions = [],
  inline = false,
  kind = "comment",
  mutationKey,
  onCancelReply,
  onClose,
  onKindChange,
  onSent,
  owner,
  replyTo = null,
  saveEdit,
}: MentionComposerProps): ReactNode {
  // Only the mount reads it, so it is computed once rather than on every keystroke.
  const [initial] = useState(() => seededDraft(seedMentions, undefined, owner));
  const textarea = useRef<HTMLTextAreaElement>(null);
  const replacementTextarea = useRef<HTMLTextAreaElement>(null);
  const formRef = useRef<HTMLFormElement>(null);
  const lastFocusedField = useRef<HTMLElement | null>(null);
  const optionLabelRefs = useRef<Array<HTMLInputElement | null>>([]);
  const focusAddedOption = useRef(false);
  const previousBody = useRef(edit?.body ?? initial.body);
  const pendingTextEdit = useRef<{ before: string; range: TextEditRange } | undefined>(undefined);
  const previousReply = useRef<string | undefined>(undefined);
  const queryClient = useQueryClient();
  const [body, setBody] = useState(edit?.body ?? initial.body);
  const [replacement, setReplacement] = useState("");
  const [mentions, setMentions] = useState<AcceptedMention[]>(initial.mentions);
  /** Puts a whole draft in the field at once - its text, mentions, and replacement in step with
   *  the request that supplied them - where the reader did not type it, so their next edit is
   *  measured from here (`previousBody`). */
  const replaceDraft = useCallback((draft: Draft) => {
    setBody(draft.body);
    previousBody.current = draft.body;
    setMentions([...draft.mentions]);
    setReplacement(draft.replacement ?? "");
  }, []);
  const [askOptions, setAskOptions] = useState<AskOptionDraft[]>(() => [emptyAskOption()]);
  const [multiple, setMultiple] = useState(false);
  const [urgency, setUrgency] = useState<AskUrgency>("med");
  const [referencePickerOpen, setReferencePickerOpen] = useState(false);
  const [autocomplete, setAutocomplete] = useState<{ query: string; start: number }>();
  const [pendingUploads, setPendingUploads] = useState(0);
  const [confirmingDiscard, setConfirmingDiscard] = useState(false);
  // Why the last kind pick was refused, kept with the mark it answered: the composer stays mounted
  // when a newer compose replaces the anchor, and a refusal about the old mark says nothing about
  // the new one.
  const [kindRefusal, setKindRefusal] = useState<{ markId: string; text: string }>();
  const autocompleteOpen = autocomplete !== undefined;
  const live = useAgents(
    suppliedAgents === undefined && owner.kind !== "session",
    autocompleteOpen
  );
  const agents = suppliedAgents ?? live.agents;
  const options = useMemo(() => mentionOptions(agents), [agents]);
  const filteredOptions = useMemo(() => {
    if (autocomplete === undefined) return [];
    const query = autocomplete.query.toLocaleLowerCase();
    return options.filter(
      (option) =>
        query === "" ||
        option.target.toLocaleLowerCase().includes(query) ||
        option.title.toLocaleLowerCase().includes(query) ||
        option.detail.toLocaleLowerCase().includes(query)
    );
  }, [autocomplete, options]);
  const compact = inline || edit !== undefined;
  /** The whole draft, gone - the body, its accepted mentions, a suggestion's replacement and an
   *  ask's options - on a successful send and on Discard alike, in every host, leaving what a fresh
   *  mount shows: a host whose `onClose` only moves focus (`AgentsPage`'s rows) keeps the composer
   *  mounted, and it must read as a new one would. That includes the channel's own mentions: an
   *  issue comment an Agents row writes reaches its agent only by mentioning it, so every message
   *  after the first is seeded as the first was. The kind, the urgency and `Allow multiple` are
   *  the reader's settings, not the draft, and stay. */
  const clearDraft = () => {
    replaceDraft(seededDraft(seedMentions, undefined, owner));
    setAskOptions([emptyAskOption()]);
  };
  const references = useMemo(() => composerReferences(body), [body]);
  // This composer's own hold, from Send's task until the server answers. It is never shared: a
  // composer that mounts while another one's send is out holds nothing for it.
  const submitGuard = useSubmitGuard();
  const uploadRetryGuard = useSubmitGuard();
  const editBody = edit?.body;

  // The seed belongs to the channel the host addresses. When it changes - an Agents row's issue
  // pick, or a reply that moves the message to another channel - the live draft takes the new
  // seed in place (`seededDraft`), the way a reply edits it: a remount would part the draft from
  // this composer's own send, its refusal and its uploads. A layout effect, so no frame shows the
  // old seed under the new channel; the reply prefill, a passive effect, still lands after it.
  const seedKey = [owner.kind, ...seedMentions.map((mention) => mention.target)].join("\n");
  const seededFor = useRef(seedKey);
  // biome-ignore lint/correctness/useExhaustiveDependencies: `seedKey` is the trigger; the draft, the seed and the owner are read as they stand at the change
  useLayoutEffect(() => {
    if (seededFor.current === seedKey) return;
    seededFor.current = seedKey;
    replaceDraft(seededDraft(seedMentions, { body, mentions }, owner));
  }, [seedKey]);
  useEffect(() => {
    if (editBody === undefined) return;
    replaceDraft({ body: editBody, mentions: [] });
  }, [editBody, replaceDraft]);
  useEffect(() => {
    if (autoFocus) textarea.current?.focus();
  }, [autoFocus]);
  useEffect(() => {
    if (focusAddedOption.current) {
      optionLabelRefs.current[askOptions.length - 1]?.focus();
      focusAddedOption.current = false;
    }
  }, [askOptions.length]);
  useEffect(() => {
    if (suppliedAgents === undefined && autocompleteOpen) void live.refetch();
  }, [autocompleteOpen, live.refetch, suppliedAgents]);
  useEffect(() => {
    if (replyTo === null) {
      previousReply.current = undefined;
      return;
    }
    if (previousReply.current === replyTo.id) return;
    previousReply.current = replyTo.id;
    const initial = replyTo.mentions ?? [];
    const deduplicated = initial.filter(
      (mention, index) =>
        initial.findIndex((candidate) => candidate.target === mention.target) === index
    );
    const texts = deduplicated.map((mention) =>
      mentionText(mentionDisplay(mention.title, mention.target))
    );
    const value = texts.join(" ");
    let offset = 0;
    replaceDraft({
      body: value,
      mentions: deduplicated.map((mention, index) => {
        const text = mentionDisplay(mention.title, mention.target);
        const start = offset;
        offset += texts[index]?.length ?? 0;
        const end = offset;
        offset += 1;
        return { end, start, target: mention.target, text };
      }),
    });
    textarea.current?.focus();
  }, [replaceDraft, replyTo]);

  /** A send the server took. The caches refreshed are the ones the send wrote, named by its own
   *  request. The draft is the composer's own, so it clears to what the channel it addresses now
   *  seeds. */
  const landed = ({ anchor: sentAnchor, edit: sentEdit, owner: sentOwner }: SentRequest) => {
    clearDraft();
    onSent();
    if (sentOwner.kind === "session") {
      void queryClient.invalidateQueries({
        queryKey: agentMessagesQuery(sentOwner.sessionId).queryKey,
      });
    } else {
      void queryClient.invalidateQueries({
        queryKey:
          sentOwner.kind === "issue"
            ? ["comments", sentOwner.issueKey]
            : ["artifact", sentOwner.artifactId, "comments"],
      });
      void queryClient.invalidateQueries({ queryKey: ["inbox"] });
      if (sentAnchor !== undefined) {
        void queryClient.invalidateQueries({
          queryKey: ["artifact", sentAnchor.artifact, "blocks"],
        });
      }
      if (sentOwner.kind === "issue") {
        void queryClient.invalidateQueries({ queryKey: ["events", sentOwner.issueKey] });
        void queryClient.invalidateQueries({ queryKey: ["issue", sentOwner.issueKey] });
      } else {
        void queryClient.invalidateQueries({ queryKey: ["artifact", sentOwner.artifactId] });
        void queryClient.invalidateQueries({
          queryKey: ["project", sentOwner.project, "artifacts"],
        });
      }
    }
    if (!inline && sentEdit === undefined) onClose();
  };
  // A send past its deadline, waiting for its answer under `pastDeadlineKey`. The composer still
  // holds its draft, and a host's hold on the send's name - a Reply, an issue pick - still counts
  // it. Only a hold `untilDeadline` lets go - a host's Back, Escape and Collapse thread - so a
  // request the server never answers cannot keep the reader in front of it.
  const late = useMutation({
    mutationFn: ({ answer }: { answer: Promise<unknown>; sent: SentRequest }) => answer,
    mutationKey: mutationKey === undefined ? undefined : pastDeadlineKey(mutationKey),
    onError: (_error, { sent }) => replaceDraft(sent.draft),
    onSettled: () => submitGuard.release(),
    onSuccess: (_data, { sent }) => landed(sent),
  });
  const save = useMutation({
    mutationFn: sendWithinDeadline,
    mutationKey,
    onError: (error, sent) => {
      // Past its deadline the request is not refused: it goes on as `late`, whose answer is the
      // send's outcome.
      if (error instanceof SendDeadlineError) {
        late.mutate({ answer: error.answer, sent });
        return;
      }
      // A refusal restores the complete draft the request turned down, not a later edit: its
      // body, accepted mentions, and suggestion replacement stay in step for Retry or editing.
      replaceDraft(sent.draft);
    },
    onSettled: (_data, error) => {
      if (!(error instanceof SendDeadlineError)) submitGuard.release();
    },
    onSuccess: (_data, sent) => landed(sent),
  });
  const sending = save.isPending || late.isPending;
  // What the server refused, for the reader: a deadline is not a refusal, and the answer that
  // follows one is.
  const refusal = late.isError
    ? late.error
    : save.isError && !(save.error instanceof SendDeadlineError)
      ? save.error
      : undefined;
  // Closed, with no send out and no refusal to show: nothing of this composer is on screen.
  const dormant = closed && !sending && refusal === undefined;
  useEffect(() => {
    const form = formRef.current;
    if (form === null || dormant) return;
    const handleEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key !== "Escape") return;
      // A message on its way is the server's until it answers. The submit guard becomes held in
      // the same task as Send, before mutation state can re-render, so Escape cannot raise a
      // Discard prompt in that gap.
      if (submitGuard.held()) {
        event.preventDefault();
        return;
      }
      if (autocomplete !== undefined) {
        event.stopPropagation();
        setAutocomplete(undefined);
        return;
      }
      if (referencePickerOpen) {
        event.stopPropagation();
        setReferencePickerOpen(false);
        return;
      }
      if (replyTo !== null) {
        event.preventDefault();
        onCancelReply?.();
        return;
      }
      event.stopPropagation();
      if (!hasUnsavedInput(body, replacement, askOptions) || confirmingDiscard) {
        onClose();
        return;
      }
      const active = document.activeElement;
      lastFocusedField.current =
        active instanceof HTMLElement && form.contains(active) ? active : null;
      setConfirmingDiscard(true);
    };
    form.addEventListener("keydown", handleEscape);
    return () => form.removeEventListener("keydown", handleEscape);
  }, [
    askOptions,
    autocomplete,
    body,
    confirmingDiscard,
    // A dormant composer renders no form; the one it renders when it wakes takes the listener.
    dormant,
    onCancelReply,
    onClose,
    referencePickerOpen,
    replacement,
    replyTo,
    submitGuard,
  ]);
  const upload = useMutation({
    // An upload carries where it goes as its own variable, taken by the paste or drop that starts
    // it, as a send carries its `SentRequest`: TanStack gives a pending mutation each new render's
    // options before `mutationFn` runs, so an owner read from the closure would follow a pick made
    // in that task. Everything after - the reference, the refreshed caches, a Retry - reads the same
    // target, so the reference names the issue or document that holds the artifact.
    mutationFn: async ({ file, target }: { file: File; target: ComposerOwner }) => {
      if (target.kind === "session")
        throw new Error("The direct session channel does not support uploads.");
      const { artifact } = await uploadFile(
        target.kind === "issue" ? { issue: target.issueKey } : { project: target.project },
        file
      );
      return { artifact, target };
    },
    onMutate: () => setPendingUploads((count) => count + 1),
    onSuccess: ({ artifact, target }) => {
      const reference =
        target.kind === "issue"
          ? buildDispatchReference({ key: target.issueKey, kind: "artifact", slug: artifact.slug })
          : buildDispatchReference({
              kind: "document",
              project: target.project,
              slug: artifact.slug,
            });
      setBody((current) => {
        const next = appendReference(current, reference);
        previousBody.current = next;
        return next;
      });
      if (target.kind === "issue") {
        void queryClient.invalidateQueries({ queryKey: ["artifacts", target.issueKey] });
        void queryClient.invalidateQueries({ queryKey: ["issue", target.issueKey] });
      } else {
        void queryClient.invalidateQueries({ queryKey: ["project", target.project, "artifacts"] });
      }
    },
    onSettled: () => {
      setPendingUploads((count) => count - 1);
      uploadRetryGuard.release();
    },
  });

  const addMention = (option: MentionOption) => {
    const current = textarea.current?.value ?? body;
    const query = mentionQuery(current, textarea.current?.selectionStart ?? null);
    if (query === undefined || mentions.some((mention) => mention.target === option.target)) {
      setAutocomplete(undefined);
      return;
    }
    const display = mentionDisplay(option.title, option.target);
    const text = mentionText(display);
    const caret = textarea.current?.selectionStart ?? current.length;
    const after = `${current.slice(0, query.start)}${text}${current.slice(caret)}`;
    setMentions((currentMentions) => [
      ...reconcileMentions(current, after, currentMentions, { end: caret, start: query.start }),
      { end: query.start + text.length, start: query.start, target: option.target, text: display },
    ]);
    previousBody.current = after;
    setBody(after);
    setAutocomplete(undefined);
    requestAnimationFrame(() => {
      textarea.current?.focus();
      textarea.current?.setSelectionRange(query.start + text.length, query.start + text.length);
    });
  };
  const onBodyChange = (value: string, selectionStart: number | null) => {
    const before = previousBody.current;
    const pending = pendingTextEdit.current;
    pendingTextEdit.current = undefined;
    setMentions((current) =>
      reconcileMentions(
        before,
        value,
        current,
        pending?.before === before ? pending.range : undefined
      )
    );
    previousBody.current = value;
    setBody(value);
    if (edit !== undefined || owner.kind === "session") {
      setAutocomplete(undefined);
      return;
    }
    setAutocomplete(mentionQuery(value, selectionStart));
    setConfirmingDiscard(false);
  };
  const onBeforeBodyInput = (event: FormEvent<HTMLTextAreaElement>) => {
    const input = event.nativeEvent as InputEvent;
    const element = event.currentTarget;
    let start = element.selectionStart ?? 0;
    let end = element.selectionEnd ?? start;
    if (start === end && input.inputType === "deleteContentBackward")
      start = Math.max(0, start - 1);
    if (start === end && input.inputType === "deleteContentForward")
      end = Math.min(element.value.length, end + 1);
    pendingTextEdit.current = { before: element.value, range: { end, start } };
  };
  const addAskOption = () => {
    focusAddedOption.current = true;
    setAskOptions((current) => [...current, emptyAskOption()]);
    setConfirmingDiscard(false);
  };
  const updateAskOption = (index: number, field: "description" | "label", value: string) => {
    setAskOptions((current) =>
      current.map((option, currentIndex) =>
        currentIndex === index ? { ...option, [field]: value } : option
      )
    );
    setConfirmingDiscard(false);
  };
  /** The send as it stands, for `sendRequest`: the fields' own text, since a keystroke in Send's
   *  task may not have rendered yet, and every prop and setting that addresses or shapes it. */
  const sentRequest = (): SentRequest => ({
    anchor,
    ask: { multiple, options: submittedAskOptions(askOptions), urgency },
    draft: {
      body: textarea.current?.value ?? body,
      mentions,
      replacement: replacementTextarea.current?.value ?? replacement,
    },
    edit:
      edit === undefined
        ? undefined
        : { id: edit.id, save: saveEdit ?? ((id, text) => api.editComment(id, { body: text })) },
    kind,
    owner,
    replyTo,
  });
  const activeMentions = useMemo(() => survivingMentions(body, mentions), [body, mentions]);
  const inheritedDelivery =
    owner.kind === "session"
      ? (replyTo?.thread?.delivery ?? "steer")
      : replyTo?.parentKind === "message"
        ? replyTo.thread?.delivery
        : activeMentions.length > 0
          ? "steer"
          : undefined;
  const outbound = deliveryPlan(body, inheritedDelivery);
  // A closed owner takes no send: the composer is only still here for a send of its own that is
  // out, or its refusal, and Send says why it cannot go.
  const submitReason = closed
    ? "This issue is closed. Reopen it to send."
    : draftRefusal(kind, body, replacement, outbound);
  const footId = useId();
  const canSubmit = canSubmitComposer(submitReason, sending, pendingUploads);
  /** Sends the draft as it stands, its whole request frozen here (`sentRequest`), so nothing done
   *  after this call can change where it goes. Until the server answers, one fieldset holds every
   *  control that could change or discard the draft; the submit guard covers that hold in this
   *  task before React can disable the fieldset. */
  const send = () =>
    submitGuard.guard(() => {
      setConfirmingDiscard(false);
      setAutocomplete(undefined);
      setReferencePickerOpen(false);
      late.reset();
      save.mutate(sentRequest());
    });
  const stopHeldComposerInput = (event: SyntheticEvent) => {
    if (!submitGuard.held()) return;
    event.preventDefault();
    event.stopPropagation();
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (canSubmit) send();
  };
  /** Drops the draft and a refusal with it. Offered only while no send is out - the prompt never
   *  shows then (`send` closes it, and Escape raises none), and a closed composer's Discard sits
   *  beside a refusal - so the send it resets has its answer: the refusal goes with the draft it
   *  was about. */
  const discard = () => {
    clearDraft();
    save.reset();
    late.reset();
    setConfirmingDiscard(false);
    onClose();
  };
  const bodyLabel =
    edit !== undefined
      ? "Edit comment"
      : inline
        ? "Reply"
        : kind === "ask"
          ? "Question"
          : kind === "suggestion"
            ? "Reason"
            : "Comment";
  const title = edit !== undefined ? "Save" : kind === "ask" ? "Ask" : "Send";
  const effectiveTargets: readonly string[] =
    owner.kind === "session"
      ? [`session:${owner.sessionId}`]
      : replyTo?.parentKind === "message" && replyTo.thread !== undefined
        ? [replyTo.thread.target]
        : activeMentions.map((mention) => mention.target);
  const outboundMode = outbound.delivery;
  const unsupportedOptions =
    outboundMode === undefined
      ? []
      : effectiveTargets.flatMap((target) => {
          const option = options.find((candidate) => candidate.target === target);
          return option !== undefined && !option.capabilities.includes(outboundMode)
            ? [option]
            : [];
        });
  const unsupported = unsupportedOptions.length > 0;
  // The prefixed modes a person could send in instead: only one every target refusing this mode
  // advertises, so the warning never suggests a mode those sessions refuse too (an aside-only
  // session is offered /aside, never /btw).
  const alternatives = (["btw", "aside"] as const).filter(
    (mode) =>
      unsupported &&
      mode !== outboundMode &&
      unsupportedOptions.every((option) => option.capabilities.includes(mode))
  );

  const composerControls = (
    <fieldset
      className={
        compact
          ? "min-w-0 space-y-2 border-0 p-0"
          : docked
            ? "min-w-0 border-0 p-0"
            : "min-w-0 space-y-3 border-0 p-0"
      }
      disabled={sending}
      onChangeCapture={stopHeldComposerInput}
      onClickCapture={stopHeldComposerInput}
      onInputCapture={stopHeldComposerInput}
      onKeyDownCapture={stopHeldComposerInput}
    >
      {/* The title repeats the submit button two rows below it, so the row exists only where it
          also carries Close - an anchored composer floating over a quote, where naming what is
          being written is worth a line. */}
      {compact || anchor === undefined ? null : (
        <div className="flex items-center justify-between gap-3">
          <p className={`text-sm font-semibold ${textPrimaryOnSurface}`}>{title}</p>
          <button className={`text-sm ${dismissButtonText}`} onClick={onClose} type="button">
            Close
          </button>
        </div>
      )}
      {replyTo === null ? null : (
        <div className="flex items-center gap-1">
          <ReplyQuote className="min-w-0 flex-1" to={replyTo.to}>
            {replyQuoteText(replyTo.author, replyTo.excerpt)}
          </ReplyQuote>
          {/* The reply is part of the message's address, fixed once the send is out, so the
              composer's control fieldset holds it with every other draft control. */}
          <button
            aria-label="Cancel reply"
            className={`inline-flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-lg text-lg leading-none disabled:cursor-not-allowed disabled:opacity-50 md:min-h-8 md:min-w-8 ${textMutedOnSurfaceMuted}`}
            onClick={onCancelReply}
            type="button"
          >
            ×
          </button>
        </div>
      )}
      {compact || anchor === undefined ? null : (
        <blockquote className={`border-l-2 pl-2 text-sm ${quoteAccentBorder} ${quoteBodyText}`}>
          {anchor.quote}
        </blockquote>
      )}
      {onKindChange !== undefined && edit === undefined && owner.kind !== "session" ? (
        <>
          <fieldset className="flex gap-1">
            <legend className="sr-only">Kind</legend>
            {(["comment", "suggestion", "ask"] as const).map((next) => (
              <button
                aria-pressed={kind === next}
                className={`min-h-11 rounded-lg px-3 text-sm ${kind === next ? primaryButtonBg : textSecondaryOnSurface}`}
                key={next}
                onClick={() => {
                  const refused = onKindChange(next);
                  setKindRefusal(
                    refused === undefined || anchor === undefined
                      ? undefined
                      : { markId: anchor.mark_id, text: refused }
                  );
                }}
                type="button"
              >
                {next === "comment" ? "Comment" : next === "suggestion" ? "Suggest" : "Ask"}
              </button>
            ))}
          </fieldset>
          {/* Why the kind did not change, where the reader pressed; a switch that takes clears it,
              and a newer anchor leaves it behind. */}
          {kindRefusal === undefined || kindRefusal.markId !== anchor?.mark_id ? null : (
            <p className={`text-sm ${dangerText}`} role="status">
              {kindRefusal.text}
            </p>
          )}
        </>
      ) : null}
      {confirmingDiscard ? (
        <div
          className={`flex items-center gap-3 rounded-lg border p-2 text-sm ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
          role="alert"
        >
          <span>Discard draft?</span>
          <button className="font-semibold underline" onClick={discard} type="button">
            Discard
          </button>
          <button
            className="underline"
            onClick={() => {
              setConfirmingDiscard(false);
              lastFocusedField.current?.focus();
            }}
            type="button"
          >
            Keep
          </button>
        </div>
      ) : null}
      {kind === "suggestion" ? (
        <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
          Replace with
          <textarea
            aria-label="Replacement"
            className={`mt-1 block w-full rounded-lg border px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
            onChange={(event) => {
              setReplacement(event.target.value);
              setConfirmingDiscard(false);
            }}
            onKeyDown={submitOnModifiedEnter}
            ref={replacementTextarea}
            value={replacement}
          />
        </label>
      ) : null}
      <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
        {compact ? null : bodyLabel}
        <textarea
          aria-label={bodyLabel}
          className={`mt-1 block w-full rounded-lg border px-3 py-2 font-normal outline-none ${inline ? "min-h-11 resize-none" : "min-h-24"} ${inputClasses(true)} ${textPrimaryOnSurface}`}
          maxLength={kind === "ask" ? 800 : 2000}
          onBeforeInput={onBeforeBodyInput}
          onChange={(event) => onBodyChange(event.target.value, event.target.selectionStart)}
          onDrop={
            compact || owner.kind === "session"
              ? undefined
              : (event: DragEvent<HTMLTextAreaElement>) => {
                  event.preventDefault();
                  for (const file of event.dataTransfer.files)
                    upload.mutate({ file, target: owner });
                }
          }
          onInput={
            inline
              ? (event: SyntheticEvent<HTMLTextAreaElement>) => {
                  event.currentTarget.style.height = "auto";
                  event.currentTarget.style.height = `${event.currentTarget.scrollHeight}px`;
                }
              : undefined
          }
          onKeyDown={(event: KeyboardEvent<HTMLTextAreaElement>) => {
            if (
              owner.kind === "issue" &&
              !compact &&
              (event.ctrlKey || event.metaKey) &&
              event.key.toLowerCase() === "k"
            ) {
              event.preventDefault();
              if (!submitGuard.held()) setReferencePickerOpen(true);
              return;
            }
            submitOnModifiedEnter(event);
          }}
          onPaste={
            compact || owner.kind === "session"
              ? undefined
              : (event: ClipboardEvent<HTMLTextAreaElement>) => {
                  const files = [...event.clipboardData.files];
                  if (files.length > 0) {
                    event.preventDefault();
                    for (const file of files) upload.mutate({ file, target: owner });
                  }
                }
          }
          ref={textarea}
          rows={1}
          value={body}
        />
      </label>
      {activeMentions.length === 0 ? null : (
        <ul
          aria-label="Mention targets"
          className={`flex flex-wrap gap-2 text-xs ${textPrimaryOnSurface}`}
        >
          {activeMentions.map((mention) => (
            <li className="rounded-full border px-2 py-1" key={mention.target}>
              {mention.text} · <code>{mention.target}</code>
            </li>
          ))}
        </ul>
      )}
      {autocomplete === undefined ? null : (
        <div
          aria-label="Mention suggestions"
          className={`max-h-52 overflow-y-auto rounded-lg border p-1 ${surfaceRecessedBg}`}
          role="listbox"
        >
          {filteredOptions.map((option) => (
            // Below `xl` every button is `inline-flex` with centred items (styles.css's touch
            // rule, unlayered, so it beats `block`): the title and detail stack explicitly and
            // each stretches to the row's width.
            <button
              aria-label={option.title}
              className={`flex min-h-11 w-full flex-col rounded-lg px-3 py-2 text-left text-sm ${surfaceMutedHoverBg}`}
              key={option.target}
              onMouseDown={(event) => event.preventDefault()}
              onClick={() => addMention(option)}
              role="option"
              type="button"
            >
              <span className={`self-stretch font-medium ${textPrimaryOnSurface}`}>
                {option.title}
              </span>
              <TruncatedText
                className={`self-stretch text-xs ${textMutedOnSurface}`}
                title={option.detail}
              >
                {option.detail}
              </TruncatedText>
            </button>
          ))}
          {filteredOptions.length === 0 ? (
            <p className={`px-3 py-2 text-sm ${textMutedOnSurface}`}>No live agents match.</p>
          ) : null}
        </div>
      )}
      {unsupported ? (
        <p className={`text-sm ${dangerText}`}>
          {unsupportedOptions.map((option) => option.title).join(" and ")}{" "}
          {unsupportedOptions.length > 1 ? "do" : "does"} not advertise{" "}
          {outboundMode === undefined ? null : MODE_LABELS[outboundMode]}, so sending it records a
          failed attempt.
          {alternatives.length === 0
            ? null
            : ` Prefix with ${alternatives.map((mode) => DELIVERY_COMMANDS[mode]).join(" or ")} to send it as ${alternatives
                .map((mode) => `${mode === "aside" ? "an" : "a"} ${MODE_LABELS[mode]}`)
                .join(" or ")} instead.`}
        </p>
      ) : null}
      {kind === "ask" ? (
        <>
          <fieldset>
            <legend className={`text-sm font-medium ${textSecondaryOnSurface}`}>Urgency</legend>
            <div className="mt-1 flex flex-wrap gap-1">
              {ASK_URGENCIES_ASCENDING.map((value) => (
                <Chip
                  key={value}
                  onClick={() => setUrgency(value)}
                  selected={urgency === value}
                  tone={value}
                >
                  {URGENCY_LABELS[value]}
                </Chip>
              ))}
            </div>
          </fieldset>
          <label
            className={`flex items-center gap-2 text-sm font-medium ${textSecondaryOnSurface}`}
          >
            <input
              checked={multiple}
              className={`size-4 rounded ${borderStrong} ${checkboxAccent} ${focusRing}`}
              onChange={(event) => setMultiple(event.target.checked)}
              type="checkbox"
            />
            Allow multiple
          </label>
          <section aria-label="Options" className="space-y-2">
            <div className="flex items-center justify-between gap-3">
              <p className={`text-sm font-medium ${textSecondaryOnSurface}`}>Options</p>
              <button
                className={`text-sm font-medium ${linkText} ${linkHoverText}`}
                onClick={addAskOption}
                type="button"
              >
                Add option
              </button>
            </div>
            {askOptions.map((option, index) => (
              <div className="grid grid-cols-[minmax(0,1fr)_auto] gap-2" key={option.id}>
                <div className="space-y-2">
                  <input
                    aria-label={`Option ${index + 1} label`}
                    className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
                    onChange={(event) => updateAskOption(index, "label", event.target.value)}
                    onKeyDown={(event) => {
                      if (event.key === "Enter") {
                        event.preventDefault();
                        addAskOption();
                      }
                    }}
                    placeholder="Label"
                    ref={(element) => {
                      optionLabelRefs.current[index] = element;
                    }}
                    value={option.label}
                  />
                  <input
                    aria-label={`Option ${index + 1} description`}
                    className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
                    onChange={(event) => updateAskOption(index, "description", event.target.value)}
                    placeholder="Description (optional)"
                    value={option.description}
                  />
                </div>
                <button
                  aria-label={`Remove option ${index + 1}`}
                  className={`self-start rounded-lg border px-3 py-2 text-sm font-medium ${borderStrong} ${secondaryButtonText} ${dangerHoverBorder} ${hoverToDangerText}`}
                  onClick={() =>
                    setAskOptions((current) =>
                      current.filter((_option, currentIndex) => currentIndex !== index)
                    )
                  }
                  type="button"
                >
                  Remove
                </button>
              </div>
            ))}
          </section>
        </>
      ) : null}
      {compact || owner.kind === "session" ? null : (
        <>
          <p className={`text-xs ${textMutedOnSurfaceMuted}`}>
            Paste or drop a file to add it as an artifact.
            {owner.kind === "issue" ? " Ctrl+K inserts a reference." : ""}
          </p>
          {references.length === 0 ? null : (
            <section aria-label="References" className="flex flex-wrap gap-2">
              {references.map((reference) => (
                <a
                  className={`rounded-full border px-2 py-1 text-xs font-medium ${referencePillBorder} ${surfaceRecessedBg} ${badgeMed.text} ${controlHoverBorder}`}
                  href={reference.href}
                  key={reference.reference}
                >
                  {reference.reference}
                </a>
              ))}
            </section>
          )}
          {/* A picker is a draft control. `send` closes one already open, and Ctrl+K consults the
              submit guard before it can open another in the task before the fieldset disables. */}
          {owner.kind === "issue" && referencePickerOpen ? (
            <ReferencePicker
              issueKey={owner.issueKey}
              onClose={() => setReferencePickerOpen(false)}
              onSelect={(reference) => {
                setBody((current) => {
                  const next = appendReference(current, reference);
                  previousBody.current = next;
                  return next;
                });
                setReferencePickerOpen(false);
                textarea.current?.focus();
              }}
            />
          ) : null}
          {pendingUploads > 0 ? (
            <p className={`text-sm ${textMutedOnSurfaceMuted}`} role="status">
              Uploading file…
            </p>
          ) : null}
          {upload.isError ? (
            <QueryError
              message={uploadErrorMessage(upload.error)}
              onRetry={() => uploadRetryGuard.retryLast(upload)}
              retrying={upload.isPending}
            />
          ) : null}
        </>
      )}
      {/* Past its deadline a send is still the server's: no Retry, which would post it twice. */}
      {late.isPending ? (
        <p className={`text-sm ${textMutedOnSurfaceMuted}`} role="status">
          Still sending — the server has not answered in {SEND_DEADLINE_MS / 1000} seconds. The
          draft stays here until it does.
        </p>
      ) : null}
      {refusal === undefined ? null : (
        <div className="flex flex-wrap items-center gap-3">
          <QueryError
            message={
              refusal instanceof ApiError &&
              refusal.status === 409 &&
              refusal.code === "ANCHOR_MISSING"
                ? refusal.message
                : `Couldn't send — ${apiErrorMessage(refusal, "network error")}`
            }
            // Retry sends the draft as it stands, so it asks what Send asks of it.
            onRetry={canSubmit ? send : undefined}
            retrying={sending}
          />
          {/* A closed owner takes no Retry, so the refusal's own way out is to drop the draft it
              handed back, once the reader has it. */}
          {closed ? (
            <button
              className={`text-sm font-medium underline ${dangerText}`}
              onClick={discard}
              type="button"
            >
              Discard draft
            </button>
          ) : null}
        </div>
      )}
      <RefusableButton
        busy={sending ? "Sending…" : pendingUploads > 0 ? "Uploading file…" : undefined}
        refusal={submitReason ?? null}
        refusalShownBy={footId}
        type="submit"
      >
        {title}
      </RefusableButton>
      {/* While Send refuses, the line under it says why - on screen, for a reader with no
          pointer to hover it - and once the draft can go, how to send it. */}
      <p className={`text-xs ${textMutedOnSurfaceMuted}`} id={footId}>
        {submitReason ?? "Ctrl/Cmd+Enter to send · Enter for a new line"}
      </p>
    </fieldset>
  );
  // A closed owner's composer stays mounted, keeping its draft, and shows itself only for a send
  // of its own that is out or that send's refusal; a host's frame around it collapses with it
  // (`empty:hidden`).
  if (dormant) return null;
  return (
    <form
      aria-label="Comment composer"
      className={
        compact
          ? ""
          : docked
            ? `${autocompleteOpen ? "z-20" : "z-[8]"} fixed inset-x-0 bottom-16 border-t px-4 py-2 sm:sticky sm:top-0 sm:z-20 sm:-mx-2 sm:border-b sm:px-2 ${borderDefault} ${surfaceBg}`
            : `rounded-xl border p-3 ${calloutInfoBorder} ${calloutInfoBg}`
      }
      onSubmit={submit}
      ref={formRef}
    >
      {composerControls}
    </form>
  );
}
