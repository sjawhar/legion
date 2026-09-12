import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  borderDefault,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { Upload } from "../artifacts/Upload";
import { ApprovalChip } from "../doc/ApprovalChip";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";

function documentUpdatedAt(document: Artifact): string {
  return document.versions.at(-1)?.created_at ?? document.created_at;
}

function DocumentRow({ document }: { document: Artifact }): ReactNode {
  return (
    <li
      aria-label={document.name}
      className={`grid grid-cols-[minmax(0,1fr)_auto] gap-x-4 gap-y-2 border-t py-3 sm:grid-cols-[minmax(0,1fr)_auto_auto_auto] sm:items-center ${borderDefault}`}
    >
      <div className="min-w-0">
        {document.issue_key === null ? (
          <Link
            className={`block truncate font-medium ${linkText} ${linkHoverText}`}
            to={buildProjectPath({
              kind: "document",
              project: document.project,
              slug: document.slug,
            })}
          >
            {document.name}
          </Link>
        ) : (
          <Link
            className={`block truncate font-medium ${linkText} ${linkHoverText}`}
            to={buildIssuePath({
              key: document.issue_key,
              kind: "artifact",
              slug: document.slug,
            })}
          >
            {document.name}
          </Link>
        )}
        <ApprovalChip artifact={document} />
      </div>
      <span className={`text-xs ${textSecondaryOnCanvas}`}>{document.kind}</span>
      <span className="text-xs">
        {document.issue_key === null ? null : (
          <Link
            className={`rounded-full border px-2 py-1 font-medium ${borderDefault} ${surfaceMutedBg} ${linkText} ${linkHoverText}`}
            to={buildIssuePath({ key: document.issue_key, kind: "issue" })}
          >
            {document.issue_key}
          </Link>
        )}
      </span>
      <Timestamp at={documentUpdatedAt(document)} className={`text-xs ${textMutedOnCanvas}`} />
    </li>
  );
}

export function DocumentList({ project }: { project: string }): ReactNode {
  const [creating, setCreating] = useState(false);
  const [title, setTitle] = useState("");
  const [duplicateName, setDuplicateName] = useState<string | undefined>();
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const navigate = useNavigate();
  const documents = useQuery({
    queryKey: ["project", project, "artifacts"],
    queryFn: () => api.listProjectArtifacts(project, true),
  });
  const create = useMutation({
    mutationFn: (name: string) => api.uploadArtifact({ project }, { content: `# ${name}\n`, name }),
    onSettled: () => submitGuard.release(),
    onSuccess: ({ artifact }) => {
      setCreating(false);
      setTitle("");
      void queryClient.invalidateQueries({ queryKey: ["project", project, "artifacts"] });
      navigate(buildProjectPath({ kind: "document", project, slug: artifact.slug }));
    },
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const name = title.trim();
    if (name === "") {
      return;
    }
    if ((documents.data ?? []).some((document) => document.name === name)) {
      setDuplicateName(name);
      return;
    }
    setDuplicateName(undefined);
    submitGuard.guard(() => create.mutate(name));
  };

  if (documents.isPending) {
    return <p className={textMutedOnCanvas}>Loading documents…</p>;
  }
  if (documents.isError) {
    return <p className={dangerText}>Could not load documents.</p>;
  }

  return (
    <section aria-label="Project documents" className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <button
          className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-semibold ${secondaryButtonBorder} ${secondaryButtonText}`}
          onClick={() => {
            setCreating(true);
            setDuplicateName(undefined);
          }}
          type="button"
        >
          New document
        </button>
      </div>
      {creating ? (
        <form aria-label="New document" className="space-y-3" onSubmit={submit}>
          <label className={`block text-sm font-medium ${textSecondaryOnCanvas}`}>
            Title
            <input
              className={`mt-1 block min-h-11 w-full rounded-lg px-3 py-2 text-sm font-normal ${inputClasses(true)}`}
              onChange={(event) => {
                setTitle(event.target.value);
                setDuplicateName(undefined);
              }}
              required
              value={title}
            />
          </label>
          {duplicateName === undefined ? null : (
            <p className={dangerText} role="alert">
              A document named {duplicateName} already exists
            </p>
          )}
          {create.isError ? (
            <p className={dangerText} role="alert">
              {create.error instanceof Error
                ? create.error.message
                : "Could not create the document."}
            </p>
          ) : null}
          <button
            className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
            disabled={create.isPending}
            type="submit"
          >
            {create.isPending ? "Creating…" : "Create"}
          </button>
        </form>
      ) : null}
      <Upload owner={{ project }} />
      {(documents.data ?? []).length === 0 ? (
        <p className={textMutedOnCanvas}>No documents yet.</p>
      ) : (
        <ul>
          {(documents.data ?? []).map((document) => (
            <DocumentRow document={document} key={document.id} />
          ))}
        </ul>
      )}
    </section>
  );
}
