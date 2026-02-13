export interface CategoryConfig {
  description: string;
  systemPrompt?: string;
  defaultModel: string;
  temperature: number;
}

export type CategoryOverrideConfig = Partial<
  Pick<CategoryConfig, "defaultModel" | "description" | "systemPrompt">
> & { model?: string };

export type ResolvedCategoryConfig = CategoryConfig & { model: string };

const DEFAULT_CATEGORIES: Record<string, CategoryConfig> = {
  "visual-engineering": {
    defaultModel: "google/gemini-3-pro",
    temperature: 0.5,
    description: "Frontend, UI/UX, design",
  },
  ultrabrain: {
    defaultModel: "anthropic/claude-opus-4-6",
    temperature: 0.5,
    description: "Hard logic-heavy tasks",
  },
  deep: {
    defaultModel: "openai/gpt-5.2-codex",
    temperature: 0.5,
    description: "Autonomous problem-solving",
  },
  artistry: {
    defaultModel: "anthropic/claude-opus-4-6",
    temperature: 0.5,
    description: "Creative approaches",
  },
  quick: {
    defaultModel: "anthropic/claude-sonnet-4-20250514",
    temperature: 0.5,
    description: "Trivial tasks",
  },
  "unspecified-low": {
    defaultModel: "anthropic/claude-sonnet-4-20250514",
    temperature: 0.5,
    description: "Low effort",
  },
  "unspecified-high": {
    defaultModel: "anthropic/claude-opus-4-6",
    temperature: 0.5,
    description: "High effort",
  },
  writing: {
    defaultModel: "anthropic/claude-sonnet-4-20250514",
    temperature: 0.5,
    description: "Documentation, prose",
  },
  "review-architect": {
    defaultModel: "anthropic/claude-sonnet-4-20250514",
    temperature: 0.3,
    description: "Review architect output",
    systemPrompt:
      "You are reviewing architect output. Evaluate acceptance criteria quality, completeness, and testability. Do NOT edit any files or run any commands — read and analyze only.",
  },
  "review-plan": {
    defaultModel: "anthropic/claude-sonnet-4-20250514",
    temperature: 0.3,
    description: "Review plan output",
    systemPrompt:
      "You are reviewing an implementation plan. Evaluate against requirements, feasibility, and dependency structure. Do NOT edit any files or run any commands — read and analyze only.",
  },
  "review-implementation": {
    defaultModel: "anthropic/claude-sonnet-4-20250514",
    temperature: 0.3,
    description: "Review implementation",
    systemPrompt:
      "You are reviewing an implementation. Evaluate spec compliance and code quality. Do NOT edit any files or run any commands — read and analyze only.",
  },
};

export function resolveCategory(
  category: string,
  userConfig?: Record<string, CategoryOverrideConfig>,
  modelOverride?: string
): ResolvedCategoryConfig {
  const fallback = DEFAULT_CATEGORIES[category] ?? DEFAULT_CATEGORIES["unspecified-low"];
  const override = userConfig?.[category];
  const overrideModel = override?.model ?? override?.defaultModel;
  const merged = override ? { ...fallback, ...override } : fallback;
  return {
    ...merged,
    model: modelOverride ?? overrideModel ?? merged.defaultModel,
  };
}
