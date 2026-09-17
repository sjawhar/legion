import type { ReactNode } from "react";

import type { Project } from "../../api/types";
import { inputClasses, textPrimaryOnSurface } from "../../theme/classes";

/** The "Choose a project" picker both Settings forms use; `disabled` is omitted from the DOM
 *  when not given. */
export function ProjectSelect({
  disabled,
  id,
  onChange,
  projects,
  value,
}: {
  disabled?: boolean;
  id: string;
  onChange: (project: string) => void;
  projects: readonly Project[];
  value: string;
}): ReactNode {
  return (
    <select
      className={`rounded-md px-3 py-2 ${inputClasses(false)} ${textPrimaryOnSurface}`}
      disabled={disabled}
      id={id}
      onChange={(event) => onChange(event.target.value)}
      required
      value={value}
    >
      <option disabled value="">
        Choose a project
      </option>
      {projects.map((candidate) => (
        <option key={candidate.key} value={candidate.key}>
          {candidate.key} · {candidate.name}
        </option>
      ))}
    </select>
  );
}
