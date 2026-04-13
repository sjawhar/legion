/**
 * Model fallback chain — an ordered list of models to try on transient failure.
 *
 * Part of #276 / #200: OMO Replacement (T7 + T6 integration point).
 */

export interface ParsedModel {
  providerID: string;
  modelID: string;
}

export interface ModelFallbackChain {
  /** The ordered list of model strings (e.g. "anthropic/claude-sonnet-4-20250514"). */
  readonly models: readonly string[];

  /** Number of models in the chain. */
  readonly length: number;

  /** The primary (first) model, or undefined if empty. */
  primary(): string | undefined;

  /** All models after the primary (the fallback list). */
  fallbacks(): string[];

  /** Parse the primary model into providerID/modelID, or undefined if empty. */
  parsePrimary(): ParsedModel | undefined;

  /** Parse any model string into providerID/modelID. */
  parseModel(model: string): ParsedModel;
}

/**
 * Parse a "provider/model" string into { providerID, modelID }.
 * If no slash is present, uses the full string for both fields.
 */
function parseModelString(model: string): ParsedModel {
  const slashIdx = model.indexOf("/");
  if (slashIdx < 0) {
    return { providerID: model, modelID: model };
  }
  return {
    providerID: model.slice(0, slashIdx),
    modelID: model.slice(slashIdx + 1),
  };
}

/**
 * Create an ordered model fallback chain.
 *
 * @param models - Ordered list of model strings. First is primary, rest are fallbacks.
 * @param fallbackModel - Optional single fallback model from RetryConfig.
 *   Appended to the chain if not already present.
 */
export function createModelFallbackChain(
  models: string[],
  fallbackModel?: string
): ModelFallbackChain {
  const chain = [...models];

  // Append fallbackModel if provided and not already in the list
  if (fallbackModel && !chain.includes(fallbackModel)) {
    chain.push(fallbackModel);
  }

  return {
    models: chain,
    length: chain.length,

    primary(): string | undefined {
      return chain[0];
    },

    fallbacks(): string[] {
      return chain.slice(1);
    },

    parsePrimary(): ParsedModel | undefined {
      const first = chain[0];
      if (!first) return undefined;
      return parseModelString(first);
    },

    parseModel(model: string): ParsedModel {
      return parseModelString(model);
    },
  };
}
