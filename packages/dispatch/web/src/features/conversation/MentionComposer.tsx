import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type ClipboardEvent,
  type DragEvent,
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
  type SyntheticEvent,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { ApiError, api, apiErrorMessage } from "../../api/client";
import type {
  Agent,
  AskOption,
  AskUrgency,
  CreateCommentInput,
  DeliveryCapability,
} from "../../api/types";
import { Chip } from "../../components/Chip";
import { QueryError } from "../../components/QueryError";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
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
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
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
import {
  buildDispatchReference,
  buildIssuePath,
  buildProjectPath,
  type DispatchRoute,
  isProjectRoute,
  parseDispatchReference,
  parseIssuePath,
  parseProjectPath,
} from "../refs/routes";
import { ReplyQuote, replyQuoteText } from "./ReplyQuote";
import { useAgents } from "./useAgents";

export type ComposerOwner =
  | { readonly kind: "issue"; readonly issueKey: string }
  | { readonly kind: "artifact"; readonly artifactId: string; readonly project: string }
  | { readonly kind: "session"; readonly sessionId: string };

export type ComposerKind = "ask" | "comment" | "suggestion";
export interface ComposerAnchor {
  readonly artifact: string;
  readonly mark_id: string;
  readonly quote: string;
}

export interface MentionReplyTarget {
  readonly author: string;
  readonly excerpt: string;
  readonly id: string;
  readonly parentKind: "comment" | "message";
  /** Canonical targets carried by a comment parent, prefilled as editable @ mentions. */
  readonly mentions?: readonly { readonly target: string; readonly title: string }[];
  /** Kept while old targeted-message threads remain readable; S2 writes comments only. */
  readonly thread?: {
    readonly delivery: DeliveryCapability;
    readonly target: string;
    readonly title: string;
  };
  readonly to?: string;
}

export type ReplyTarget = MentionReplyTarget;

interface TextEditRange {
  readonly end: number;
  readonly start: number;
}

interface AcceptedMention {
  readonly end: number;
  readonly start: number;
  readonly target: string;
  readonly text: string;
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

export interface ComposerReference {
  href?: string;
  reference: string;
}

export function trimReference(value: string): string {
  return value.replace(/[),.;:!?]+$/, "");
}

function composerReference(route: DispatchRoute): ComposerReference | undefined {
  if (isProjectRoute(route)) {
    return route.kind === "document"
      ? { href: buildProjectPath(route), reference: buildDispatchReference(route) }
      : undefined;
  }
  return { href: buildIssuePath(route), reference: buildDispatchReference(route) };
}

function appReference(value: string, appOrigin: string): ComposerReference | undefined {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return undefined;
  }
  if (url.origin !== appOrigin) return undefined;
  const route =
    parseIssuePath(url.pathname, url.search) ?? parseProjectPath(url.pathname, url.search);
  return route === undefined ? undefined : composerReference(route);
}

export function composerReferences(
  body: string,
  appOrigin = window.location.origin
): ComposerReference[] {
  const references: ComposerReference[] = [];
  for (const raw of body.match(/(?:dispatch:\/\/|https?:\/\/)\S+/g) ?? []) {
    const value = trimReference(raw);
    const reference = value.startsWith("dispatch://")
      ? (() => {
          const route = parseDispatchReference(value);
          return route === undefined ? undefined : composerReference(route);
        })()
      : appReference(value, appOrigin);
    if (
      reference !== undefined &&
      !references.some((item) => item.reference === reference.reference)
    ) {
      references.push(reference);
    }
  }
  return references;
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

function parseDelivery(body: string): { body: string; delivery: DeliveryCapability } {
  const match = /^(\/btw |\/aside )/.exec(body);
  if (match === null) return { body, delivery: "steer" };
  return { body: body.slice(match[0].length), delivery: match[0] === "/btw " ? "btw" : "aside" };
}

interface DeliveryPlan {
  readonly body: string;
  readonly delivery: DeliveryCapability | undefined;
}

function deliveryPlan(body: string, inherited: DeliveryCapability | undefined): DeliveryPlan {
  const parsed = parseDelivery(body);
  const hasCommand = parsed.body !== body;
  return {
    body: inherited === undefined || !hasCommand ? body : parsed.body,
    delivery: inherited === undefined ? undefined : hasCommand ? parsed.delivery : inherited,
  };
}

function mentionText(text: string): string {
  return `@${text}`;
}

function mentionDisplay(title: string, target: string): string {
  const trimmed = title.trim();
  if (trimmed !== "" && !/^(?:session|role):/i.test(trimmed) && !/[@\r\n]/.test(trimmed)) {
    return trimmed;
  }
  return target.replace(":", " ");
}

function survivingMentions(body: string, mentions: readonly AcceptedMention[]): AcceptedMention[] {
  const seen = new Set<string>();
  const surviving: AcceptedMention[] = [];
  for (const mention of mentions) {
    if (
      seen.has(mention.target) ||
      mention.start < 0 ||
      mention.end > body.length ||
      mention.start >= mention.end ||
      body.slice(mention.start, mention.end) !== mentionText(mention.text)
    ) {
      continue;
    }
    seen.add(mention.target);
    surviving.push(mention);
  }
  return surviving;
}
function hasDraft(kind: ComposerKind, body: string, replacement: string): boolean {
  return (kind === "suggestion" ? replacement : body).trim().length > 0;
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

export function canSubmitComposer(
  kind: ComposerKind,
  body: string,
  replacement: string,
  isSaving: boolean,
  pendingUploads: number
): boolean {
  return hasDraft(kind, body, replacement) && !isSaving && pendingUploads === 0;
}

interface MentionComposerProps {
  readonly agents?: readonly Agent[];
  readonly anchor?: ComposerAnchor;
  readonly autoFocus?: boolean;
  readonly docked?: boolean;
  readonly edit?: { readonly body: string; readonly id: string };
  readonly initialMentions?: readonly { readonly target: string; readonly title: string }[];
  readonly inline?: boolean;
  readonly kind?: ComposerKind;
  readonly onCancelReply?: () => void;
  readonly onClose: () => void;
  readonly onSent: () => void;
  readonly owner: ComposerOwner;
  readonly replyTo?: MentionReplyTarget | null;
  readonly saveEdit?: (id: string, body: string) => Promise<unknown>;
  /** A selected document mark is the only surface where the kind can change. */
  readonly showKindSwitch?: boolean;
}

export function MentionComposer({
  agents: suppliedAgents,
  anchor,
  autoFocus = false,
  docked = false,
  edit,
  initialMentions = [],
  inline = false,
  kind: initialKind = "comment",
  onCancelReply,
  onClose,
  onSent,
  owner,
  replyTo = null,
  saveEdit,
  showKindSwitch = false,
}: MentionComposerProps): ReactNode {
  const initialBody = initialMentions
    .map((mention) => mentionText(mentionDisplay(mention.title, mention.target)))
    .join(" ");
  const textarea = useRef<HTMLTextAreaElement>(null);
  const replacementTextarea = useRef<HTMLTextAreaElement>(null);
  const formRef = useRef<HTMLFormElement>(null);
  const lastFocusedField = useRef<HTMLElement | null>(null);
  const optionLabelRefs = useRef<Array<HTMLInputElement | null>>([]);
  const focusAddedOption = useRef(false);
  const previousBody = useRef(edit?.body ?? initialBody);
  const pendingTextEdit = useRef<{ before: string; range: TextEditRange } | undefined>(undefined);
  const previousReply = useRef<string | undefined>(undefined);
  const queryClient = useQueryClient();
  const [body, setBody] = useState(edit?.body ?? initialBody);
  const [replacement, setReplacement] = useState("");
  const [kind, setKind] = useState<ComposerKind>(initialKind);
  const [mentions, setMentions] = useState<AcceptedMention[]>(() => {
    let offset = 0;
    return initialMentions.map((mention) => {
      const text = mentionDisplay(mention.title, mention.target);
      const start = offset;
      offset += mentionText(text).length;
      const end = offset;
      offset += 1;
      return { end, start, target: mention.target, text };
    });
  });
  const [askOptions, setAskOptions] = useState<AskOptionDraft[]>(() => [emptyAskOption()]);
  const [multiple, setMultiple] = useState(false);
  const [urgency, setUrgency] = useState<AskUrgency>("med");
  const [referencePickerOpen, setReferencePickerOpen] = useState(false);
  const [autocomplete, setAutocomplete] = useState<{ query: string; start: number }>();
  const [pendingUploads, setPendingUploads] = useState(0);
  const [confirmingDiscard, setConfirmingDiscard] = useState(false);
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
  const references = useMemo(() => composerReferences(body), [body]);
  const submitGuard = useSubmitGuard();
  const uploadRetryGuard = useSubmitGuard();
  const editBody = edit?.body;

  useEffect(() => {
    if (editBody === undefined) return;
    setBody(editBody);
    previousBody.current = editBody;
    setMentions([]);
  }, [editBody]);
  useEffect(() => {
    setKind(initialKind);
  }, [initialKind]);
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
    setBody(value);
    previousBody.current = value;
    setMentions(
      deduplicated.map((mention, index) => {
        const text = mentionDisplay(mention.title, mention.target);
        const start = offset;
        offset += texts[index]?.length ?? 0;
        const end = offset;
        offset += 1;
        return { end, start, target: mention.target, text };
      })
    );
    textarea.current?.focus();
  }, [replyTo]);
  useEffect(() => {
    const form = formRef.current;
    if (form === null) return;
    const handleEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key !== "Escape") return;
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
    onCancelReply,
    onClose,
    referencePickerOpen,
    replacement,
    replyTo,
  ]);

  const selection =
    anchor === undefined ? undefined : { artifact: anchor.artifact, mark_id: anchor.mark_id };
  const commentQueryKey =
    owner.kind === "issue"
      ? ["comments", owner.issueKey]
      : owner.kind === "artifact"
        ? ["artifact", owner.artifactId, "comments"]
        : undefined;
  const save = useMutation({
    mutationFn: async ({
      body: draft,
      replacement: nextReplacement,
    }: {
      body: string;
      replacement: string;
    }) => {
      if (edit !== undefined) {
        return saveEdit === undefined
          ? api.editComment(edit.id, { body: draft.trim() })
          : saveEdit(edit.id, draft.trim());
      }
      if (kind === "ask") {
        if (owner.kind === "session")
          throw new Error("The direct session channel does not support asks.");
        const input = {
          anchor: selection,
          multiple,
          options: submittedAskOptions(askOptions),
          question: draft.trim(),
          urgency,
        };
        return owner.kind === "issue"
          ? api.createAsk(owner.issueKey, input)
          : api.createArtifactAsk(owner.artifactId, input);
      }
      if (owner.kind === "session") {
        const inheritedDelivery = replyTo?.thread?.delivery ?? "steer";
        const plan = deliveryPlan(draft, inheritedDelivery);
        const reply = replyTo === null ? {} : { in_reply_to: replyTo.id };
        return api.createAgentMessage(owner.sessionId, {
          body: plan.body,
          delivery: plan.delivery ?? inheritedDelivery,
          ...reply,
        });
      }
      if (replyTo?.parentKind === "message") {
        if (owner.kind !== "issue") throw new Error("Legacy message replies belong to an issue.");
        const plan = deliveryPlan(draft, replyTo.thread?.delivery);
        return api.createMessage(owner.issueKey, {
          body: plan.body,
          in_reply_to: replyTo.id,
          ...(replyTo.thread === undefined
            ? {}
            : { delivery: plan.delivery, target: replyTo.thread.target }),
        });
      }
      const targets = survivingMentions(draft, mentions).map((mention) => mention.target);
      const baseBody =
        kind === "suggestion" && draft.trim() === "" ? "Suggested replacement." : draft;
      const plan = deliveryPlan(baseBody, targets.length === 0 ? undefined : "steer");
      const comment: CreateCommentInput = {
        body: plan.body,
        ...(replyTo === null ? {} : { reply_to: replyTo.id }),
        ...(kind === "suggestion" ? { suggestion: { replace_with: nextReplacement } } : {}),
        ...(targets.length === 0
          ? {}
          : {
              delivery: plan.delivery ?? "steer",
              mentions: targets.map((target) => ({ target })),
            }),
      };
      const input = replyTo === null ? { ...comment, anchor: selection } : comment;
      return owner.kind === "issue"
        ? api.createComment(owner.issueKey, input)
        : api.createArtifactComment(owner.artifactId, input);
    },
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      setBody("");
      previousBody.current = "";
      setMentions([]);
      onSent();
      if (commentQueryKey !== undefined)
        void queryClient.invalidateQueries({ queryKey: commentQueryKey });
      if (owner.kind === "session") {
        void queryClient.invalidateQueries({ queryKey: ["agents", owner.sessionId, "messages"] });
      } else {
        void queryClient.invalidateQueries({ queryKey: ["inbox"] });
        if (anchor !== undefined) {
          void queryClient.invalidateQueries({ queryKey: ["artifact", anchor.artifact, "blocks"] });
        }
        if (owner.kind === "issue") {
          void queryClient.invalidateQueries({ queryKey: ["events", owner.issueKey] });
          void queryClient.invalidateQueries({ queryKey: ["issue", owner.issueKey] });
        } else {
          void queryClient.invalidateQueries({ queryKey: ["artifact", owner.artifactId] });
          void queryClient.invalidateQueries({ queryKey: ["project", owner.project, "artifacts"] });
        }
      }
      if (!inline && edit === undefined) onClose();
    },
  });
  const upload = useMutation({
    mutationFn: (file: File) => {
      if (owner.kind === "session")
        throw new Error("The direct session channel does not support uploads.");
      return uploadFile(
        owner.kind === "issue" ? { issue: owner.issueKey } : { project: owner.project },
        file
      );
    },
    onMutate: () => setPendingUploads((count) => count + 1),
    onSuccess: ({ artifact }) => {
      if (owner.kind === "session") return;
      const reference =
        owner.kind === "issue"
          ? buildDispatchReference({ key: owner.issueKey, kind: "artifact", slug: artifact.slug })
          : buildDispatchReference({
              kind: "document",
              project: owner.project,
              slug: artifact.slug,
            });
      setBody((current) => {
        const next = appendReference(current, reference);
        previousBody.current = next;
        return next;
      });
      if (owner.kind === "issue") {
        void queryClient.invalidateQueries({ queryKey: ["artifacts", owner.issueKey] });
        void queryClient.invalidateQueries({ queryKey: ["issue", owner.issueKey] });
      } else {
        void queryClient.invalidateQueries({ queryKey: ["project", owner.project, "artifacts"] });
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
  const currentDraft = () => ({
    body: textarea.current?.value ?? body,
    replacement: replacementTextarea.current?.value ?? replacement,
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
  const canSubmit =
    canSubmitComposer(kind, body, replacement, save.isPending, pendingUploads) &&
    (kind !== "comment" || outbound.delivery === undefined || outbound.body.trim() !== "");
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (canSubmit) submitGuard.guard(() => save.mutate(currentDraft()));
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

  return (
    <form
      aria-label="Comment composer"
      className={
        compact
          ? "space-y-2"
          : docked
            ? `${autocompleteOpen ? "z-20" : "z-[8]"} fixed inset-x-0 bottom-16 border-t px-4 py-2 sm:sticky sm:top-0 sm:z-20 sm:-mx-2 sm:border-b sm:px-2 ${borderDefault} ${surfaceBg}`
            : `space-y-3 rounded-xl border p-3 ${calloutInfoBorder} ${calloutInfoBg}`
      }
      onSubmit={submit}
      ref={formRef}
    >
      {compact ? null : (
        <div className="flex items-center justify-between gap-3">
          <p className={`text-sm font-semibold ${textPrimaryOnSurface}`}>{title}</p>
          {anchor === undefined ? null : (
            <button className={`text-sm ${dismissButtonText}`} onClick={onClose} type="button">
              Close
            </button>
          )}
        </div>
      )}
      {replyTo === null ? null : (
        <div className="flex items-center gap-1">
          <ReplyQuote className="min-w-0 flex-1" to={replyTo.to}>
            {replyQuoteText(replyTo.author, replyTo.excerpt)}
          </ReplyQuote>
          <button
            aria-label="Cancel reply"
            className={`inline-flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-lg text-lg leading-none md:min-h-8 md:min-w-8 ${textMutedOnSurfaceMuted}`}
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
      {showKindSwitch && edit === undefined && owner.kind !== "session" ? (
        <fieldset className="flex gap-1">
          <legend className="sr-only">Kind</legend>
          {(["comment", "suggestion", "ask"] as const).map((next) => (
            <button
              aria-pressed={kind === next}
              className={`min-h-11 rounded-lg px-3 text-sm ${kind === next ? primaryButtonBg : textSecondaryOnSurface}`}
              key={next}
              onClick={() => setKind(next)}
              type="button"
            >
              {next === "comment" ? "Comment" : next === "suggestion" ? "Suggest" : "Ask"}
            </button>
          ))}
        </fieldset>
      ) : null}
      {confirmingDiscard ? (
        <div
          className={`flex items-center gap-3 rounded-lg border p-2 text-sm ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
          role="alert"
        >
          <span>Discard draft?</span>
          <button className="font-semibold underline" onClick={onClose} type="button">
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
          disabled={save.isPending}
          maxLength={kind === "ask" ? 800 : 2000}
          onBeforeInput={onBeforeBodyInput}
          onChange={(event) => onBodyChange(event.target.value, event.target.selectionStart)}
          onDrop={
            compact || owner.kind === "session"
              ? undefined
              : (event: DragEvent<HTMLTextAreaElement>) => {
                  event.preventDefault();
                  for (const file of event.dataTransfer.files) upload.mutate(file);
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
              setReferencePickerOpen(true);
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
                    for (const file of files) upload.mutate(file);
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
            <button
              aria-label={option.title}
              className={`block min-h-11 w-full rounded-lg px-3 py-2 text-left text-sm ${surfaceMutedHoverBg}`}
              key={option.target}
              onMouseDown={(event) => event.preventDefault()}
              onClick={() => addMention(option)}
              role="option"
              type="button"
            >
              <span className={`block font-medium ${textPrimaryOnSurface}`}>{option.title}</span>
              <span className={`block truncate text-xs ${textMutedOnSurface}`}>
                {option.detail}
              </span>
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
          {outboundMode === "btw" ? "BTW" : outboundMode === "aside" ? "Aside" : "Steer"}; Send will
          record the failed attempt.
          {outboundMode === "steer"
            ? " Prefix with /btw to send as a background message instead."
            : null}
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
      {save.isError ? (
        <QueryError
          message={
            save.error instanceof ApiError &&
            save.error.status === 409 &&
            save.error.code === "ANCHOR_MISSING"
              ? save.error.message
              : `Couldn't send — ${apiErrorMessage(save.error, "network error")}`
          }
          onRetry={() => submitGuard.guard(() => save.mutate(currentDraft()))}
          retrying={save.isPending}
        />
      ) : null}
      <button
        className={`rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
        disabled={!canSubmit}
        type="submit"
      >
        {save.isPending ? "Sending…" : pendingUploads > 0 ? "Uploading file…" : title}
      </button>
      <p className={`text-xs ${textMutedOnSurfaceMuted}`}>
        Ctrl/Cmd+Enter to send · Enter for a new line
      </p>
    </form>
  );
}
