// packages/broker-client/src/source.ts

/** One platform identity read at the moment it is traded: the subject token of the broker's RFC
 *  8693 exchange, and the type the exchange reads it under. */
export interface SubjectToken {
  readonly token: string;
  readonly tokenType: string;
}

/**
 * A platform identity this process can prove, read fresh each time it is asked. The grant issues
 * no refresh token, so every renewal asks its source again — a projected token the kubelet
 * rewrote in place, a metadata token minted anew — and a source that caches must cache for less
 * than the token it answers lives.
 *
 * It mirrors the Go client's `Source` (`packages/envoy/internal/brokerclient/source.go`): the two
 * implementations here are the two identities a TypeScript caller holds. A machine that federates
 * through a workload identity pool trades through the Go client, which `agent-secrets` runs.
 */
export interface TokenSource {
  /**
   * The subject token to present and the type it is presented under, or a rejected promise that
   * never reached the broker: an identity this machine could not read is never an empty token for
   * the broker to refuse.
   */
  read(signal?: AbortSignal): Promise<SubjectToken>;
  /** Names the identity in a refusal, without naming the token: the file read, or the metadata
   *  server asked. */
  describe(): string;
}
