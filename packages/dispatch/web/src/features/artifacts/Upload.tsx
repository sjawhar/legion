import { useMutation, useQueryClient } from "@tanstack/react-query";
import { type ChangeEvent, type DragEvent, type ReactNode, useRef, useState } from "react";

import { ApiError, type ArtifactOwner, api } from "../../api/client";
import type { Artifact, Version } from "../../api/types";
import {
  borderDefault,
  borderStrong,
  dangerText,
  inputClasses,
  surfaceBg,
  surfaceMutedBg,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";

export interface UploadResult {
  artifact: Artifact;
  version: Version;
}

interface UploadFileOptions {
  summary?: string;
}

export async function uploadFile(
  owner: ArtifactOwner,
  file: File,
  options: UploadFileOptions = {}
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

interface UploadProps {
  owner: ArtifactOwner;
}

export function Upload({ owner }: UploadProps): ReactNode {
  const queryClient = useQueryClient();
  const input = useRef<HTMLInputElement>(null);
  const [summary, setSummary] = useState("");
  const upload = useMutation({
    mutationFn: (file: File) =>
      uploadFile(owner, file, { summary: summary.trim() === "" ? undefined : summary.trim() }),
    onSuccess: () => {
      if ("issue" in owner) {
        void queryClient.invalidateQueries({ queryKey: ["artifacts", owner.issue] });
        void queryClient.invalidateQueries({ queryKey: ["issue", owner.issue] });
        return;
      }
      void queryClient.invalidateQueries({ queryKey: ["project", owner.project, "artifacts"] });
    },
  });
  const selectFile = (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    if (file !== undefined) {
      upload.mutate(file);
    }
    event.target.value = "";
  };
  const dropFile = (event: DragEvent<HTMLButtonElement>) => {
    event.preventDefault();
    const file = event.dataTransfer.files[0];
    if (file !== undefined) {
      upload.mutate(file);
    }
  };

  return (
    <section className={`space-y-3 border-b py-3 ${borderDefault}`}>
      <label
        className={`block text-sm font-medium ${textSecondaryOnSurface}`}
        htmlFor="artifact-summary"
      >
        Summary (optional)
        <input
          className={`mt-1 block w-full rounded-lg px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
          id="artifact-summary"
          onChange={(event) => setSummary(event.target.value)}
          value={summary}
        />
      </label>
      <button
        className={`w-full rounded-xl border border-dashed p-4 text-center ${borderStrong} ${surfaceMutedBg}`}
        onClick={() => input.current?.click()}
        onDragOver={(event) => event.preventDefault()}
        onDrop={dropFile}
        type="button"
      >
        <span className={`block text-sm ${textSecondaryOnSurface}`}>
          Drop a file here, or choose one to upload.
        </span>
        <span
          className={`mt-3 inline-block rounded-lg border px-3 py-2 text-sm font-medium ${borderStrong} ${surfaceBg} ${textSecondaryOnSurface}`}
        >
          Choose file
        </span>
      </button>
      <input
        aria-label="Upload artifact"
        className="sr-only left-0"
        onChange={selectFile}
        ref={input}
        type="file"
      />
      {upload.isPending ? (
        <p className={`text-sm ${textMutedOnSurface}`} role="status">
          Uploading artifact…
        </p>
      ) : null}
      {upload.isError ? (
        <p className={`text-sm ${dangerText}`} role="alert">
          {uploadErrorMessage(upload.error)}
        </p>
      ) : null}
    </section>
  );
}
