import { type QueryClient, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type ChangeEvent,
  type ClipboardEvent,
  type DragEvent,
  type ReactNode,
  type RefObject,
  useRef,
  useState,
} from "react";

import { ApiError, type ArtifactOwner, api } from "../../api/client";
import type { Artifact, Version } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  calloutInfoBg,
  calloutInfoBorder,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
} from "../../theme/classes";
import { buildDispatchReference, type DispatchReferenceRoute } from "../refs/routes";

export interface UploadResult {
  artifact: Artifact;
  version: Version;
}

export async function uploadFile(
  owner: ArtifactOwner,
  file: File,
  options: { name?: string; summary?: string } = {}
): Promise<UploadResult> {
  return api.uploadArtifact(owner, {
    file,
    name: options.name ?? (file.name || "artifact"),
    summary: options.summary,
  });
}

// A refused upload is described by the server, which names the bound it hit: a markdown document
// is at most 1 MiB and any other file at most 25 MiB, so a 413 is not one message.
export function uploadErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    return error.message;
  }
  return "Could not upload the artifact.";
}

/** Refreshes the lists an upload to `owner` adds a row to. */
function refreshOwnerArtifacts(queryClient: QueryClient, owner: ArtifactOwner): void {
  if ("issue" in owner) {
    void queryClient.invalidateQueries({ queryKey: ["artifacts", owner.issue] });
    void queryClient.invalidateQueries({ queryKey: ["issue", owner.issue] });
  } else if ("project" in owner) {
    void queryClient.invalidateQueries({ queryKey: ["project", owner.project, "artifacts"] });
  } else {
    void queryClient.invalidateQueries({ queryKey: ["agent-artifacts", owner.session] });
  }
}

/** `text` added to the end of a draft, a space apart from what is already there. */
export function appendToDraft(draft: string, text: string): string {
  return `${draft}${draft.length === 0 || /\s$/.test(draft) ? "" : " "}${text}`;
}

/**
 * What an uploaded file adds to the draft it was pasted or dropped into. A picture (a file whose
 * type is `image/*`) is Markdown's image syntax captioned with the file's name and addressed by
 * its own reference at the version the upload created -
 * `![shot.png](dispatch://CORE-1/artifact/shot-png@v1)` - so the picture is part of what is sent
 * and shows wherever the text renders. Any other file is its plain reference.
 */
export function uploadedFileText(
  owner: ArtifactOwner,
  file: File,
  { artifact, version }: UploadResult
): string {
  const pinned = file.type.startsWith("image/") ? version.number : undefined;
  const route: DispatchReferenceRoute =
    "issue" in owner
      ? { key: owner.issue, kind: "artifact", slug: artifact.slug }
      : "project" in owner
        ? { kind: "document", project: owner.project, slug: artifact.slug }
        : { kind: "agent-artifact", session: owner.session, slug: artifact.slug };
  if (pinned === undefined) {
    return buildDispatchReference(route);
  }
  // A caption is link text: a bracket or backslash in a file name would end it or escape what
  // follows, a backtick would open a code span across it, and a line break would end the picture.
  const caption = (file.name || artifact.name).replace(/[\\[\]`]/g, "\\$&").replace(/\s+/g, " ");
  return `![${caption}](${buildDispatchReference({ ...route, version: pinned })})`;
}

/** Files pasted or dropped into a draft's field, each uploaded and its text added to the draft. */
export interface DraftUpload {
  /** Uploads the files a paste holds to `owner`, in place of the paste; a paste of text alone is
   *  left to the field. */
  paste(event: ClipboardEvent<HTMLElement>, owner: ArtifactOwner): void;
  /** Uploads the files a drop holds to `owner`, in place of the drop. */
  drop(event: DragEvent<HTMLElement>, owner: ArtifactOwner): void;
  /** Uploads still out. */
  pending: number;
  error: unknown;
  isError: boolean;
  isPending: boolean;
  /** Uploads the last refused file again, to the owner it was sent to. */
  retry(): void;
}

/**
 * The one upload behind a draft's field - a composer's, an ask's answer, a reply under an ask:
 * every file pasted or dropped into it uploads to the owner the paste or drop named, and once the
 * server has it, `insert` receives the text to add (`uploadedFileText`). The owner rides the
 * upload as its own mutation variable, as a send carries its request: TanStack gives a pending
 * mutation each new render's options before `mutationFn` runs, so an owner read from the closure
 * would follow a pick made in the paste's own task. The text, the refreshed lists and a Retry all
 * read that same owner.
 */
export function useDraftUpload(insert: (text: string) => void): DraftUpload {
  const queryClient = useQueryClient();
  const [pending, setPending] = useState(0);
  const retryGuard = useSubmitGuard();
  const upload = useMutation({
    mutationFn: async ({ file, owner }: { file: File; owner: ArtifactOwner }) => ({
      file,
      owner,
      result: await uploadFile(owner, file),
    }),
    onMutate: () => setPending((count) => count + 1),
    onSuccess: ({ file, owner, result }) => {
      insert(uploadedFileText(owner, file, result));
      refreshOwnerArtifacts(queryClient, owner);
    },
    onSettled: () => {
      setPending((count) => count - 1);
      retryGuard.release();
    },
  });
  const receive = (files: FileList, owner: ArtifactOwner, event: { preventDefault(): void }) => {
    if (files.length === 0) {
      return;
    }
    event.preventDefault();
    for (const file of [...files]) upload.mutate({ file, owner });
  };
  return {
    drop: (event, owner) => receive(event.dataTransfer.files, owner, event),
    error: upload.error,
    isError: upload.isError,
    isPending: upload.isPending,
    paste: (event, owner) => receive(event.clipboardData.files, owner, event),
    pending,
    retry: () => {
      retryGuard.retryLast(upload);
    },
  };
}

/** `Uploading file…` while any of a draft's uploads is out, and a refused one's reason with its
 *  Retry. */
export function DraftUploadStatus({ upload }: { upload: DraftUpload }): ReactNode {
  return (
    <>
      {upload.pending > 0 ? (
        <p className={`text-sm ${textMutedOnSurfaceMuted}`} role="status">
          Uploading file…
        </p>
      ) : null}
      {upload.isError ? (
        <QueryError
          message={uploadErrorMessage(upload.error)}
          onRetry={upload.retry}
          retrying={upload.isPending}
        />
      ) : null}
    </>
  );
}

interface ArtifactDropTarget {
  isDragging: boolean;
  onDragLeave(): void;
  onDragOver(event: DragEvent<HTMLElement>): void;
  onDrop(event: DragEvent<HTMLElement>): void;
}

export interface ArtifactUpload {
  cancel(): void;
  confirm(): void;
  dropTarget: ArtifactDropTarget;
  error: unknown;
  fileInputRef: RefObject<HTMLInputElement | null>;
  isError: boolean;
  isPending: boolean;
  openPicker(): void;
  pendingFile: File | null;
  selectFile(event: ChangeEvent<HTMLInputElement>): void;
  setSummary(value: string): void;
  summary: string;
}

/**
 * The one artifact upload: a picked or dropped file either uploads at once (`immediate`, the
 * project Documents list) or is staged until `confirm` (the Artifacts tab's row, whose summary
 * field appears once there is something to summarize and whose "Cancel" discards the pick).
 * `name` uploads every file under that document name, replacing that document, rather than under
 * the picked file's own name. Success invalidates the owner's artifact list and, for an issue, the
 * issue itself.
 */
export function useArtifactUpload(
  owner: ArtifactOwner,
  { immediate = false, name }: { immediate?: boolean; name?: string } = {}
): ArtifactUpload {
  const queryClient = useQueryClient();
  const fileInputRef = useRef<HTMLInputElement>(null);
  const [pendingFile, setPendingFile] = useState<File | null>(null);
  const [summary, setSummary] = useState("");
  const [isDragging, setIsDragging] = useState(false);
  const upload = useMutation({
    mutationFn: (file: File) =>
      uploadFile(owner, file, {
        name,
        summary: summary.trim() === "" ? undefined : summary.trim(),
      }),
    onSuccess: (result) => {
      setPendingFile(null);
      setSummary("");
      void queryClient.invalidateQueries({ queryKey: ["artifact", result.artifact.id] });
      refreshOwnerArtifacts(queryClient, owner);
    },
  });
  const receive = (file: File | undefined) => {
    if (file === undefined) {
      return;
    }
    if (immediate) {
      upload.mutate(file);
    } else {
      setPendingFile(file);
    }
  };

  return {
    cancel: () => {
      setPendingFile(null);
      setSummary("");
    },
    confirm: () => {
      if (pendingFile !== null) {
        upload.mutate(pendingFile);
      }
    },
    dropTarget: {
      isDragging,
      onDragLeave: () => setIsDragging(false),
      onDragOver: (event) => {
        event.preventDefault();
        setIsDragging(true);
      },
      onDrop: (event) => {
        event.preventDefault();
        setIsDragging(false);
        receive(event.dataTransfer.files[0]);
      },
    },
    error: upload.error,
    fileInputRef,
    isError: upload.isError,
    isPending: upload.isPending,
    openPicker: () => fileInputRef.current?.click(),
    pendingFile,
    selectFile: (event) => {
      receive(event.target.files?.[0]);
      event.target.value = "";
    },
    setSummary,
    summary,
  };
}

export function ArtifactUploadRow({ upload }: { upload: ArtifactUpload }): ReactNode {
  return (
    // The host owns the strip this sits in, so the row carries no divider of its own: idle it
    // is one button beside the filter, staging a file it takes the line.
    <div
      className={`flex min-w-0 flex-wrap items-center gap-2 ${upload.pendingFile === null ? "shrink-0" : "w-full"}`}
    >
      {upload.pendingFile === null ? (
        <button
          className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          onClick={upload.openPicker}
          type="button"
        >
          Upload
        </button>
      ) : (
        <>
          <span className={`min-w-0 truncate text-sm ${textMutedOnSurface}`}>
            {upload.pendingFile.name}
          </span>
          <input
            aria-label="Summary (optional)"
            className={`min-h-11 min-w-40 flex-1 rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
            onChange={(event) => upload.setSummary(event.target.value)}
            placeholder="Summary (optional)"
            value={upload.summary}
          />
          <button
            className={`min-h-11 shrink-0 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
            disabled={upload.isPending}
            onClick={upload.confirm}
            type="button"
          >
            Upload
          </button>
          <button
            className={`min-h-11 shrink-0 text-sm font-medium ${linkText} ${linkHoverText}`}
            disabled={upload.isPending}
            onClick={upload.cancel}
            type="button"
          >
            Cancel
          </button>
        </>
      )}
      <ArtifactUploadStatus upload={upload} />
    </div>
  );
}

/** "Uploading artifact…" while in flight, else the upload error, else nothing. */
export function ArtifactUploadStatus({ upload }: { upload: ArtifactUpload }): ReactNode {
  if (upload.isPending) {
    return (
      <p className={`w-full text-sm ${textMutedOnSurface}`} role="status">
        Uploading artifact…
      </p>
    );
  }
  if (upload.isError) {
    return (
      <p className={`w-full text-sm ${dangerText}`} role="alert">
        {uploadErrorMessage(upload.error)}
      </p>
    );
  }
  return null;
}

/** The two ways to hand `upload` a file: a drop anywhere over `children`, and the one hidden
 * `Upload artifact` file input that the host's picker button opens through `openPicker`. */
export function ArtifactDropZone({
  children,
  upload,
}: {
  children: ReactNode;
  upload: ArtifactUpload;
}): ReactNode {
  const { dropTarget } = upload;
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: a passive drag-and-drop surface — the host's own picker button remains the keyboard-reachable way to add a file.
    <div
      className={`rounded-lg ${
        dropTarget.isDragging ? `border border-dashed ${calloutInfoBorder} ${calloutInfoBg}` : ""
      }`}
      onDragLeave={dropTarget.onDragLeave}
      onDragOver={dropTarget.onDragOver}
      onDrop={dropTarget.onDrop}
    >
      {children}
      <input
        aria-label="Upload artifact"
        className="sr-only left-0"
        onChange={upload.selectFile}
        ref={upload.fileInputRef}
        type="file"
      />
    </div>
  );
}
