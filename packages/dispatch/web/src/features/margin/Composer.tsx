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
  from: number;
  occurrence?: number;
  quote: string;
  to: number;
}

export type ComposerKind = "ask" | "comment" | "suggestion";

interface ComposerProps {
  anchor: ComposerAnchor;
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
export function hasUnsavedInput(body: string, replacement: string): boolean {
  return body.trim().length > 0 || replacement.trim().length > 0;
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
  const formRef = useRef<HTMLFormElement>(null);
  const lastFocusedField = useRef<HTMLTextAreaElement | null>(null);
  const [body, setBody] = useState("");
  const [replacement, setReplacement] = useState("");
  const [pickerOpen, setPickerOpen] = useState(false);
  const [pendingUploads, setPendingUploads] = useState(0);
  const [confirmingDiscard, setConfirmingDiscard] = useState(false);
  useEffect(() => {
    if (autoFocus) {
      textarea.current?.focus();
    }
  }, [autoFocus]);
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
      if (!hasUnsavedInput(body, replacement) || confirmingDiscard) {
        onClose();
        return;
      }
      lastFocusedField.current =
        document.activeElement === replacementTextarea.current
          ? replacementTextarea.current
          : textarea.current;
      setConfirmingDiscard(true);
    };
    form.addEventListener("keydown", handleEscape);
    return () => form.removeEventListener("keydown", handleEscape);
  }, [body, confirmingDiscard, onClose, replacement]);
  const references = useMemo(() => composerReferences(body), [body]);
  const save = useMutation({
    mutationFn: async () => {
      const selection = {
        artifact: anchor.artifact,
        from: anchor.from,
        occurrence: anchor.occurrence,
        quote: anchor.quote,
        to: anchor.to,
      };
      if (kind === "ask") {
        return api.createAsk(issueKey, { anchor: selection, question: body.trim() });
      }
      return api.createComment(issueKey, {
        anchor: selection,
        body: kind === "suggestion" && body.trim() === "" ? "Suggested replacement." : body.trim(),
        reply_to: replyTo,
        suggestion: kind === "suggestion" ? { replace_with: replacement } : undefined,
      });
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["comments", issueKey] });
      void queryClient.invalidateQueries({ queryKey: ["inbox"] });
      void queryClient.invalidateQueries({ queryKey: ["issue", issueKey] });
      onClose();
    },
  });
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
    },
  });

  const addReference = (reference: string) => {
    setBody((current) => appendReference(current, reference));
    setPickerOpen(false);
    textarea.current?.focus();
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
  const handleKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "k") {
      event.preventDefault();
      setPickerOpen(true);
    }
  };
  const canSubmit = canSubmitComposer(kind, body, replacement, save.isPending, pendingUploads);
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (canSubmit) {
      save.mutate();
    }
  };
  const title = kind === "ask" ? "Ask" : kind === "suggestion" ? "Suggest" : "Comment";

  return (
    <form
      aria-label={`${title} composer`}
      className="space-y-3 rounded-xl border border-sky-200 bg-sky-50 p-3"
      onSubmit={submit}
      ref={formRef}
    >
      <div className="flex items-center justify-between gap-3">
        <p className="text-sm font-semibold text-sky-950">{title}</p>
        <button
          className="text-sm text-slate-600 hover:text-slate-950"
          onClick={onClose}
          type="button"
        >
          Close
        </button>
      </div>
      <blockquote className="border-l-2 border-sky-500 pl-2 text-sm text-slate-700">
        {anchor.quote}
      </blockquote>
      {confirmingDiscard ? (
        <div
          className="flex items-center gap-3 rounded-lg border border-amber-300 bg-amber-50 p-2 text-sm text-amber-900"
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
        <label className="block text-sm font-medium text-slate-700">
          Replace with
          <textarea
            aria-label="Replacement"
            className="mt-1 block w-full rounded-lg border border-slate-300 bg-white px-3 py-2 font-normal outline-none focus:border-sky-500"
            onChange={(event) => {
              setReplacement(event.target.value);
              setConfirmingDiscard(false);
            }}
            ref={replacementTextarea}
            value={replacement}
          />
        </label>
      ) : null}
      <label className="block text-sm font-medium text-slate-700">
        {kind === "ask" ? "Question" : kind === "suggestion" ? "Reason (optional)" : "Comment"}
        <textarea
          aria-label={kind === "ask" ? "Question" : kind === "suggestion" ? "Reason" : "Comment"}
          className="mt-1 block min-h-24 w-full rounded-lg border border-slate-300 bg-white px-3 py-2 font-normal outline-none focus:border-sky-500"
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
      <p className="text-xs text-slate-500">
        Paste or drop a file to add it as an artifact. Ctrl+K inserts a reference.
      </p>
      {references.length === 0 ? null : (
        <section aria-label="References" className="flex flex-wrap gap-2">
          {references.map((reference) => (
            <a
              className="rounded-full border border-sky-300 bg-white px-2 py-1 text-xs font-medium text-sky-800 hover:border-sky-500"
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
        <p className="text-sm text-slate-500" role="status">
          Uploading file…
        </p>
      ) : null}
      {upload.isError ? (
        <p className="text-sm text-rose-700">{uploadErrorMessage(upload.error)}</p>
      ) : null}
      {save.isError ? (
        <p className="text-sm text-rose-700">
          {save.error instanceof ApiError &&
          save.error.status === 409 &&
          save.error.code === "ANCHOR_STALE"
            ? save.error.message
            : "Could not save this item."}
        </p>
      ) : null}
      <button
        className="rounded-lg bg-sky-600 px-3 py-2 text-sm font-semibold text-white enabled:hover:bg-sky-700 disabled:cursor-not-allowed disabled:bg-slate-300"
        disabled={!canSubmit}
        type="submit"
      >
        {save.isPending ? "Saving…" : pendingUploads > 0 ? "Uploading file…" : title}
      </button>
    </form>
  );
}
