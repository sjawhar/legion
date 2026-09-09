import { useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type ClipboardEvent,
  type DragEvent,
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
  useMemo,
  useRef,
  useState,
} from "react";

import { api } from "../../api/client";
import { uploadErrorMessage, uploadFile } from "../artifacts/Upload";
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
  quote: string;
  to: number;
}

export type ComposerKind = "ask" | "comment" | "suggestion";

interface ComposerProps {
  anchor: ComposerAnchor;
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

function filesFromPaste(event: ClipboardEvent<HTMLTextAreaElement>): File[] {
  return [...event.clipboardData.files].filter((file) => file.type.startsWith("image/"));
}

function filesFromDrop(event: DragEvent<HTMLTextAreaElement>): File[] {
  return [...event.dataTransfer.files].filter((file) => file.type.startsWith("image/"));
}

export function canSubmitComposer(
  kind: ComposerKind,
  body: string,
  replacement: string,
  isSaving: boolean,
  pendingUploads: number
): boolean {
  return (
    (kind === "suggestion" ? replacement.trim().length > 0 : body.trim().length > 0) &&
    !isSaving &&
    pendingUploads === 0
  );
}

export function Composer({ anchor, kind, issueKey, onClose, replyTo }: ComposerProps): ReactNode {
  const queryClient = useQueryClient();
  const textarea = useRef<HTMLTextAreaElement>(null);
  const [body, setBody] = useState("");
  const [replacement, setReplacement] = useState("");
  const [pickerOpen, setPickerOpen] = useState(false);
  const [pendingUploads, setPendingUploads] = useState(0);
  const currentIssue = useQuery({
    enabled: pickerOpen,
    queryKey: ["issue", issueKey],
    queryFn: () => api.getIssue(issueKey),
  });
  const pickerIssues = useQuery({
    enabled: pickerOpen && currentIssue.data !== undefined,
    queryKey: ["issues", currentIssue.data?.project],
    queryFn: () => api.listIssues({ project: currentIssue.data?.project }),
  });
  const pickerArtifacts = useQueries({
    queries: (pickerIssues.data ?? []).map((issue) => ({
      enabled: pickerOpen,
      queryKey: ["artifacts", issue.key],
      queryFn: () => api.listArtifacts(issue.key),
    })),
  });
  const references = useMemo(() => composerReferences(body), [body]);
  const save = useMutation({
    mutationFn: async () => {
      const selection = { artifact: anchor.artifact, from: anchor.from, to: anchor.to };
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
    const files = filesFromPaste(event);
    if (files.length > 0) {
      event.preventDefault();
      uploadFiles(files);
    }
  };
  const handleDrop = (event: DragEvent<HTMLTextAreaElement>) => {
    event.preventDefault();
    uploadFiles(filesFromDrop(event));
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
      {kind === "suggestion" ? (
        <label className="block text-sm font-medium text-slate-700">
          Replace with
          <textarea
            aria-label="Replacement"
            className="mt-1 block w-full rounded-lg border border-slate-300 bg-white px-3 py-2 font-normal outline-none focus:border-sky-500"
            onChange={(event) => setReplacement(event.target.value)}
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
          onChange={(event) => setBody(event.target.value)}
          onDrop={handleDrop}
          onKeyDown={handleKeyDown}
          onPaste={handlePaste}
          ref={textarea}
          value={body}
        />
      </label>
      <p className="text-xs text-slate-500">
        Paste or drop an image to add it as an artifact. Ctrl+K inserts a reference.
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
        <section
          aria-label="Reference picker"
          className="space-y-2 rounded-lg border border-slate-200 bg-white p-2"
        >
          {pickerIssues.isPending ? (
            <p className="text-sm text-slate-500">Loading issues…</p>
          ) : null}
          {(pickerIssues.data ?? []).map((issue, index) => (
            <div className="space-y-1" key={issue.key}>
              <button
                className="block w-full rounded px-2 py-1 text-left text-sm hover:bg-slate-100"
                onClick={() =>
                  addReference(buildDispatchReference({ key: issue.key, kind: "issue" }))
                }
                type="button"
              >
                {issue.key}: {issue.title}
              </button>
              {pickerArtifacts[index]?.isPending ? (
                <p className="px-2 text-xs text-slate-500">Loading artifacts…</p>
              ) : null}
              {(pickerArtifacts[index]?.data ?? []).map((artifact) => (
                <button
                  className="ml-3 block w-[calc(100%-0.75rem)] rounded px-2 py-1 text-left text-sm hover:bg-slate-100"
                  key={artifact.id}
                  onClick={() =>
                    addReference(
                      buildDispatchReference({
                        key: issue.key,
                        kind: "artifact",
                        slug: artifact.slug,
                      })
                    )
                  }
                  type="button"
                >
                  {artifact.name}
                </button>
              ))}
            </div>
          ))}
        </section>
      ) : null}
      {pendingUploads > 0 ? (
        <p className="text-sm text-slate-500" role="status">
          Uploading image…
        </p>
      ) : null}
      {upload.isError ? (
        <p className="text-sm text-rose-700">{uploadErrorMessage(upload.error)}</p>
      ) : null}
      {save.isError ? <p className="text-sm text-rose-700">Could not save this item.</p> : null}
      <button
        className="rounded-lg bg-sky-600 px-3 py-2 text-sm font-semibold text-white enabled:hover:bg-sky-700 disabled:cursor-not-allowed disabled:bg-slate-300"
        disabled={!canSubmit}
        type="submit"
      >
        {save.isPending ? "Saving…" : pendingUploads > 0 ? "Uploading image…" : title}
      </button>
    </form>
  );
}
