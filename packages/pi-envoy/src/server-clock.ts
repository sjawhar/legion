import type { EnvoyFetch } from "@legion/envoy-client/transport";

/**
 * How far this machine's clock runs from the Envoy listener's, read from the `Date` header of the
 * listener's replies.
 *
 * A card's `at:` is not the sending agent's time: the listener the sender sent through sets it
 * (`issued_at`, `packages/envoy/cmd/listener/api.go`), and Dispatch's server sets it on a Dispatch
 * event. Its `delivered:` is this machine's time (`./card-age`). So the gap between the two crosses
 * exactly two clocks, and a gap that this machine's clock alone made reads like a wait unless the
 * card says how the two clocks stand. Every listener reply carries the listener's clock in its
 * `Date` header (Go's server writes one on every response), and every heartbeat registers again, so
 * this machine compares its clock with the listener's every heartbeat at the latest.
 *
 * The measurement is the reply's arrival time on this machine less its `Date`, which is truncated to
 * the second and written before the reply travels: it overstates this machine's lead by up to the
 * reply's travel time plus one second. Only this session's listener is measured: a sending server
 * with another clock (Dispatch's, or a listener on the sender's own machine) is taken to keep it.
 */
export class ServerClock {
  readonly #now: () => number;
  #offset: number | undefined;

  constructor(now: () => number) {
    this.#now = now;
  }

  /** `fetch` as given, reading each reply's `Date` before it hands the reply on. */
  observing(fetch: EnvoyFetch): EnvoyFetch {
    return async (input, init) => {
      const response = await fetch(input, init);
      this.observe(response.headers.get("date"));
      return response;
    };
  }

  /** A reply arrived now carrying `date`, its `Date` header; one with none or no time is ignored. */
  observe(date: string | null): void {
    if (date === null) return;
    const server = Date.parse(date);
    if (Number.isNaN(server)) return;
    this.#offset = this.#now() - server;
  }

  /**
   * How many milliseconds this machine's clock runs ahead of the listener's (negative: behind), from
   * the latest dated reply; undefined before one.
   */
  offset(): number | undefined {
    return this.#offset;
  }
}
