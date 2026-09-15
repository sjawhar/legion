import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type ChangeEvent,
  type DragEvent,
  type ReactNode,
  type RefObject,
  useRef,
  useState,
} from "react";

import { ApiError, type ArtifactOwner, api } from "../../api/client";
import type { Artifact, Version } from "../../api/types";
import {
  borderDefault,
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
} from "../../theme/classes";

export interface UploadResult {
  artifact: Artifact;
  version: Version;
}

export async function uploadFile(
  owner: ArtifactOwner,
  file: File,
  options: { summary?: string } = {}
): Promise<UploadResult> {
  return api.uploadArtifact(owner, {
    file,
    name: file.name || "artifact",
    summary: options.summary,
  });
}

export function uploadErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.status === 413) {
    return "file exceeds 25 MB";
  }
  if (error instanceof ApiError) {
    return error.message;
  }
  return "Could not upload the artifact.";
}

export interface ArtifactDropTarget {
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
 * Success invalidates the owner's artifact list and, for an issue, the issue itself.
 */
export function useArtifactUpload(
  owner: ArtifactOwner,
  { immediate = false }: { immediate?: boolean } = {}
): ArtifactUpload {
  const queryClient = useQueryClient();
  const fileInputRef = useRef<HTMLInputElement>(null);
  const [pendingFile, setPendingFile] = useState<File | null>(null);
  const [summary, setSummary] = useState("");
  const [isDragging, setIsDragging] = useState(false);
  const upload = useMutation({
    mutationFn: (file: File) =>
      uploadFile(owner, file, { summary: summary.trim() === "" ? undefined : summary.trim() }),
    onSuccess: () => {
      setPendingFile(null);
      setSummary("");
      if ("issue" in owner) {
        void queryClient.invalidateQueries({ queryKey: ["artifacts", owner.issue] });
        void queryClient.invalidateQueries({ queryKey: ["issue", owner.issue] });
        return;
      }
      void queryClient.invalidateQueries({ queryKey: ["project", owner.project, "artifacts"] });
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
    <div className={`flex flex-wrap items-center gap-2 border-b py-2 ${borderDefault}`}>
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
      <input
        aria-label="Upload artifact"
        className="sr-only"
        onChange={upload.selectFile}
        ref={upload.fileInputRef}
        type="file"
      />
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

export function ArtifactDropZone({
  children,
  dropTarget,
}: {
  children: ReactNode;
  dropTarget: ArtifactDropTarget;
}): ReactNode {
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
    </div>
  );
}
