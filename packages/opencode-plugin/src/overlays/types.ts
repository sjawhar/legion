export interface ModelOverlay {
  systemContent: string;
  provider: string;
}

export interface FallbackChain extends Iterable<string> {
  /** The primary model to try first. */
  readonly primary: string;
  /** Ordered list of fallback models to try if the primary fails. */
  readonly fallbacks: readonly string[];
}
