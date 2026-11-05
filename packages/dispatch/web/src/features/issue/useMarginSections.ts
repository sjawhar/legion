import { useUserPreference } from "../shell/userPreference";
import type { MarginSection } from "./page-structure";

/**
 * Which of the panel's property sections the reader has collapsed, kept for the next issue they
 * open (dispatch://INFRA-1196 R12), the way the margin's own width and the pins are kept: a
 * browser preference of the signed-in identity (`features/shell/userPreference.ts`).
 *
 * Every section opens expanded, so a first load shows every property it holds; what is stored is
 * what the reader closed, which means a section added later opens expanded too.
 */
export interface MarginSections {
  readonly isOpen: (section: MarginSection) => boolean;
  readonly toggle: (section: MarginSection) => void;
}

export function useMarginSections(): MarginSections {
  const [collapsed, setCollapsed] = useUserPreference<readonly string[]>(
    "issue.margin-sections",
    (stored) => (stored === null ? [] : stored.split(",").filter((name) => name !== "")),
    (sections) => sections.join(",")
  );
  return {
    isOpen: (section) => !collapsed.includes(section),
    toggle: (section) =>
      setCollapsed(
        collapsed.includes(section)
          ? collapsed.filter((name) => name !== section)
          : [...collapsed, section]
      ),
  };
}
