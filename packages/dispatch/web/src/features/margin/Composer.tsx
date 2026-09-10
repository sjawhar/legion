import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type ClipboardEvent,
  type DragEvent,
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { ApiError, api } from "../../api/client";
import type { AskOption, AskUrgency } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  badgeBlocking,
  badgeHigh,
  badgeLow,
  badgeMed,
  borderStrong,
  calloutInfoBg,
  calloutInfoBorder,
  calloutInfoTitleText,
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  cardHoverBorder,
  checkboxAccent,
  controlHoverBorder,
  dangerHoverBorder,
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
  secondaryButtonBorder,
  secondaryButtonText,
  surfaceRecessedBg,
  textMutedOnSurfaceMuted,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { uploadErrorMessage, uploadFile } from "../artifacts/Upload";
import { ReferencePicker } from "../refs/ReferencePicker";
import {
  buildDispatchReference,
  buildIssuePath,
  parseDispatchReference,
  parseIssuePath,
} from "../refs/routes";

export interface ComposerReference {
  href: string;
  reference: string;
}

export interface ComposerAnchor {
  artifact: string;
  occurrence?: number;
  quote: string;
}

export type ComposerKind = "ask" | "comment" | "message" | "suggestion";

interface AskOptionDraft {
  description: string;
  id: number;
  label: string;
}

const askUrgencies = [
  ["low", "Low"],
  ["med", "Medium"],
  ["high", "High"],
  ["blocking", "Blocking"],
] as const satisfies readonly (readonly [AskUrgency, string])[];

const URGENCY_BUTTON_STYLES: Record<AskUrgency, string> = {
  blocking: `${badgeBlocking.bg} ${badgeBlocking.text}`,
  high: `${badgeHigh.bg} ${badgeHigh.text}`,
  low: `${badgeLow.bg} ${badgeLow.text}`,
  med: `${badgeMed.bg} ${badgeMed.text}`,
};

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

interface ComposerProps {
  anchor?: ComposerAnchor;
  autoFocus?: boolean;
  kind: ComposerKind;
  issueKey: string;
  onClose: () => void;
  replyTo?: string;
}

function trimReference(value: string): string {
  return value.replace(/[),.;:!?]+$/, "");
}

function appReference(value: string, appOrigin: string): ComposerReference | undefined {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return undefined;
  }
  if (url.origin !== appOrigin) {
    return undefined;
  }
  const route = parseIssuePath(url.pathname, url.search);
  return route === undefined
    ? undefined
    : { href: buildIssuePath(route), reference: buildDispatchReference(route) };
}

export function composerReferences(
  body: string,
  appOrigin = window.location.origin
): ComposerReference[] {
  const references: ComposerReference[] = [];
  for (const raw of body.match(/(?:dispatch:\/\/|https?:\/\/)\S+/g) ?? []) {
    const value = trimReference(raw);
    let reference: ComposerReference | undefined;
    if (value.startsWith("dispatch://")) {
      const route = parseDispatchReference(value);
      reference =
        route === undefined
          ? undefined
          : { href: buildIssuePath(route), reference: buildDispatchReference(route) };
    } else {
      reference = appReference(value, appOrigin);
    }
    if (
      reference !== undefined &&
      !references.some((existing) => existing.reference === reference.reference)
    ) {
      references.push(reference);
    }
  }
  return references;
}

function appendReference(body: string, reference: string): string {
  return `${body}${body.length === 0 || /\s$/.test(body) ? "" : " "}${reference}`;
}

export function hasDraft(kind: ComposerKind, body: string, replacement: string): boolean {
  return (kind === "suggestion" ? replacement : body).trim().length > 0;
}

/**
 * Whether the user has typed anything at all, in any field — used to decide whether Escape
 * needs to confirm before discarding. Unlike hasDraft (which gates submit and only counts the
 * field a given kind actually saves), this counts every field so a suggestion's optional
 * "Reason" alone still triggers the discard prompt even though it alone cannot be submitted.
 */
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

export function Composer({
  anchor,
  autoFocus,
  kind,
  issueKey,
  onClose,
  replyTo,
}: ComposerProps): ReactNode {
  const queryClient = useQueryClient();
  const textarea = useRef<HTMLTextAreaElement>(null);
  const replacementTextarea = useRef<HTMLTextAreaElement>(null);
  const optionLabelRefs = useRef<Array<HTMLInputElement | null>>([]);
  const formRef = useRef<HTMLFormElement>(null);
  const lastFocusedField = useRef<HTMLElement | null>(null);
  const focusAddedOption = useRef(false);
  const [body, setBody] = useState("");
  const [replacement, setReplacement] = useState("");
  const [askOptions, setAskOptions] = useState<AskOptionDraft[]>(() => [emptyAskOption()]);
  const [multiple, setMultiple] = useState(false);
  const [urgency, setUrgency] = useState<AskUrgency>("med");
  const [pickerOpen, setPickerOpen] = useState(false);
  const [pendingUploads, setPendingUploads] = useState(0);
  const [confirmingDiscard, setConfirmingDiscard] = useState(false);
  useEffect(() => {
    if (autoFocus) {
      textarea.current?.focus();
    }
  }, [autoFocus]);
  useEffect(() => {
    if (focusAddedOption.current) {
      optionLabelRefs.current[askOptions.length - 1]?.focus();
      focusAddedOption.current = false;
    }
  }, [askOptions.length]);
  // Attached to the form (not `document`) so a nested dialog — the reference picker, or the
  // phone bottom sheet this composer can sit inside — claims Escape first during the native
  // bubble phase; only when nothing closer intercepts does closing the composer itself run.
  useEffect(() => {
    const form = formRef.current;
    if (form === null) {
      return;
    }
    const handleEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key !== "Escape") {
        return;
      }
      event.stopPropagation();
      if (pickerOpen) {
        setPickerOpen(false);
        return;
      }
      if (!hasUnsavedInput(body, replacement, askOptions) || confirmingDiscard) {
        onClose();
        return;
      }
      const activeElement = document.activeElement;
      lastFocusedField.current =
        activeElement instanceof HTMLElement && form.contains(activeElement) ? activeElement : null;
      setConfirmingDiscard(true);
    };
    form.addEventListener("keydown", handleEscape);
    return () => form.removeEventListener("keydown", handleEscape);
  }, [askOptions, body, confirmingDiscard, onClose, pickerOpen, replacement]);
  const references = useMemo(() => composerReferences(body), [body]);
  const submitGuard = useSubmitGuard();
  const save = useMutation({
    mutationFn: async () => {
      const selection =
        anchor === undefined
          ? undefined
          : {
              artifact: anchor.artifact,
              ...(anchor.occurrence === undefined ? {} : { occurrence: anchor.occurrence }),
              quote: anchor.quote,
            };
      if (kind === "message") {
        return api.createMessage(issueKey, { body: body.trim() });
      }
      if (kind === "ask") {
        return api.createAsk(issueKey, {
          anchor: selection,
          multiple,
          options: submittedAskOptions(askOptions),
          question: body.trim(),
          urgency,
        });
      }
      const comment = {
        body: kind === "suggestion" && body.trim() === "" ? "Suggested replacement." : body.trim(),
        reply_to: replyTo,
        suggestion: kind === "suggestion" ? { replace_with: replacement } : undefined,
      };
      return api.createComment(
        issueKey,
        replyTo === undefined ? { ...comment, anchor: selection } : comment
      );
    },
    onSettled: () => {
      submitGuard.release();
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["comments", issueKey] });
      void queryClient.invalidateQueries({ queryKey: ["events", issueKey] });
      void queryClient.invalidateQueries({ queryKey: ["inbox"] });
      void queryClient.invalidateQueries({ queryKey: ["issue", issueKey] });
      onClose();
    },
  });
  const uploadRetryGuard = useSubmitGuard();
  const upload = useMutation({
    mutationFn: (file: File) => uploadFile(issueKey, file),
    onMutate: () => {
      setPendingUploads((count) => count + 1);
    },
    onSuccess: ({ artifact }) => {
      setBody((current) =>
        appendReference(
          current,
          buildDispatchReference({ key: issueKey, kind: "artifact", slug: artifact.slug })
        )
      );
      void queryClient.invalidateQueries({ queryKey: ["artifacts", issueKey] });
      void queryClient.invalidateQueries({ queryKey: ["issue", issueKey] });
    },
    onSettled: () => {
      setPendingUploads((count) => count - 1);
      uploadRetryGuard.release();
    },
  });

  const addReference = (reference: string) => {
    setBody((current) => appendReference(current, reference));
    setPickerOpen(false);
    textarea.current?.focus();
  };
  const addAskOption = () => {
    focusAddedOption.current = true;
    setAskOptions((current) => [...current, emptyAskOption()]);
    setConfirmingDiscard(false);
  };
  const removeAskOption = (index: number) => {
    setAskOptions((current) => current.filter((_option, currentIndex) => currentIndex !== index));
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
  const handleOptionLabelKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Enter") {
      event.preventDefault();
      addAskOption();
    }
  };
  const uploadFiles = (files: File[]) => {
    for (const file of files) {
      upload.mutate(file);
    }
  };
  const handlePaste = (event: ClipboardEvent<HTMLTextAreaElement>) => {
    const files = [...event.clipboardData.files];
    if (files.length > 0) {
      event.preventDefault();
      uploadFiles(files);
    }
  };
  const handleDrop = (event: DragEvent<HTMLTextAreaElement>) => {
    event.preventDefault();
    uploadFiles([...event.dataTransfer.files]);
  };
  const canSubmit = canSubmitComposer(kind, body, replacement, save.isPending, pendingUploads);
  const handleKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "k") {
      event.preventDefault();
      setPickerOpen(true);
      return;
    }
    if ((event.ctrlKey || event.metaKey) && event.key === "Enter") {
      event.preventDefault();
      if (canSubmit) {
        save.mutate();
      }
    }
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (canSubmit) {
      submitGuard.guard(() => save.mutate());
    }
  };
  const title =
    kind === "ask"
      ? "Ask"
      : kind === "suggestion"
        ? "Suggest"
        : kind === "message"
          ? "Message"
          : "Comment";

  return (
    <form
      aria-label={`${title} composer`}
      className={`space-y-3 rounded-xl border p-3 ${calloutInfoBorder} ${calloutInfoBg}`}
      onSubmit={submit}
      ref={formRef}
    >
      <div className="flex items-center justify-between gap-3">
        <p className={`text-sm font-semibold ${calloutInfoTitleText}`}>{title}</p>
        {anchor === undefined ? null : (
          <button className={`text-sm ${dismissButtonText}`} onClick={onClose} type="button">
            Close
          </button>
        )}
      </div>
      {anchor === undefined ? null : (
        <blockquote className={`border-l-2 pl-2 text-sm ${quoteAccentBorder} ${quoteBodyText}`}>
          {anchor.quote}
        </blockquote>
      )}
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
            ref={replacementTextarea}
            value={replacement}
          />
        </label>
      ) : null}
      <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
        {kind === "ask"
          ? "Question"
          : kind === "suggestion"
            ? "Reason (optional)"
            : kind === "message"
              ? "Message"
              : "Comment"}
        <textarea
          aria-label={
            kind === "ask"
              ? "Question"
              : kind === "suggestion"
                ? "Reason"
                : kind === "message"
                  ? "Message"
                  : "Comment"
          }
          className={`mt-1 block min-h-24 w-full rounded-lg border px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
          maxLength={kind === "ask" ? 800 : 2000}
          onChange={(event) => {
            setBody(event.target.value);
            setConfirmingDiscard(false);
          }}
          onDrop={handleDrop}
          onKeyDown={handleKeyDown}
          onPaste={handlePaste}
          ref={textarea}
          value={body}
        />
      </label>
      {kind === "ask" ? (
        <>
          <fieldset>
            <legend className={`text-sm font-medium ${textSecondaryOnSurface}`}>Urgency</legend>
            <div className="mt-1 flex flex-wrap gap-1">
              {askUrgencies.map(([value, label]) => (
                <button
                  aria-pressed={urgency === value}
                  className={`rounded-md px-2.5 py-1 text-sm font-medium ${
                    urgency === value
                      ? URGENCY_BUTTON_STYLES[value]
                      : `border ${secondaryButtonBorder} ${surfaceRecessedBg} ${secondaryButtonText} ${cardHoverBorder}`
                  }`}
                  key={value}
                  onClick={() => setUrgency(value)}
                  type="button"
                >
                  {label}
                </button>
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
                    onKeyDown={handleOptionLabelKeyDown}
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
                  onClick={() => removeAskOption(index)}
                  type="button"
                >
                  Remove
                </button>
              </div>
            ))}
          </section>
        </>
      ) : null}
      <p className={`text-xs ${textMutedOnSurfaceMuted}`}>
        Paste or drop a file to add it as an artifact. Ctrl+K inserts a reference.
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
      {pickerOpen ? (
        <ReferencePicker
          issueKey={issueKey}
          onClose={() => setPickerOpen(false)}
          onSelect={addReference}
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
      {save.isError ? (
        <QueryError
          message={
            save.error instanceof ApiError &&
            save.error.status === 409 &&
            save.error.code === "ANCHOR_STALE"
              ? save.error.message
              : "Could not save this item."
          }
          onRetry={() => submitGuard.guard(() => save.mutate())}
          retrying={save.isPending}
        />
      ) : null}
      <button
        className={`rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
        disabled={!canSubmit}
        type="submit"
      >
        {save.isPending ? "Saving…" : pendingUploads > 0 ? "Uploading file…" : title}
      </button>
    </form>
  );
}
