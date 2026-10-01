import { expect, test } from "bun:test";
import { DELIVERY_DUPLICATE_WINDOW_MS, RECEIPT_TIMEOUT_CAUSE } from "@legion/contracts";
import { fireEvent, render, screen } from "@testing-library/react";

import { answeredWithErrorGuidance, duplicateText, safeRetryGuidance } from "./delivery";
import { type TargetedMessageAttempt, TargetedMessageCard } from "./TargetedMessageCard";

function attempt(overrides: Partial<TargetedMessageAttempt> = {}): TargetedMessageAttempt {
  return {
    attempt: 1,
    // Inside the stream's duplicate window, where a same-mode retry can still be promised safe.
    createdAt: new Date(Date.now() - 60_000).toISOString(),
    delivery: "steer",
    state: "failed",
    ...overrides,
  };
}

function card(props: {
  attempts: readonly TargetedMessageAttempt[];
  capabilities?: readonly string[];
  isClosed?: boolean;
  onRetry?: (delivery: "aside" | "btw" | "steer") => void;
}) {
  const capabilities = props.capabilities ?? ["aside", "btw", "steer"];
  return (
    <ul>
      <TargetedMessageCard
        body={<p>Ship it.</p>}
        canAside={capabilities.includes("aside")}
        canBtw={capabilities.includes("btw")}
        canSteer={capabilities.includes("steer")}
        deliveries={props.attempts}
        header={null}
        isClosed={props.isClosed ?? false}
        onRetry={props.onRetry ?? (() => {})}
        targetName="planner"
        turnID="turn-1"
      />
    </ul>
  );
}

// LEGION-271, acceptance 7. The retry row offered only BTW and steer, so retrying an aside
// attempt was always a mode change - a different idempotency key, a second delivery - offered
// under a headline promising the retry was safe. Retry re-sends in the attempt's own mode.
test("Retry re-sends an aside attempt as an aside", () => {
  const sent: string[] = [];
  const view = render(
    card({ attempts: [attempt({ delivery: "aside" })], onRetry: (mode) => void sent.push(mode) })
  );
  try {
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(sent).toEqual(["aside"]);
  } finally {
    view.unmount();
  }
});

test("Retry re-sends a steer attempt as a steer, and the mode changes stay available", () => {
  const sent: string[] = [];
  const view = render(
    card({ attempts: [attempt({ delivery: "steer" })], onRetry: (mode) => void sent.push(mode) })
  );
  try {
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    fireEvent.click(screen.getByRole("button", { name: "Use BTW instead" }));
    expect(sent).toEqual(["steer", "btw"]);
  } finally {
    view.unmount();
  }
});

// Every other button in this row is disabled with a visible reason when the recipient does not
// advertise its mode, so Retry cannot be the one that offers a send failing at resolution.
test("Retry is disabled with a visible reason when the recipient dropped the attempt's mode", () => {
  const view = render(
    card({ attempts: [attempt({ delivery: "aside" })], capabilities: ["btw", "steer"] })
  );
  try {
    expect(screen.getByRole("button", { name: "Retry" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByText("planner does not support Aside.")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// A reason that names a way out names only one the session takes: an aside-only session (a
// Claude Code session) refuses a BTW as well, so its "Use Send instead" is not told to use
// BTW, while a session that takes BTW is.
for (const [capabilities, reason] of [
  [["aside"], "planner does not support Send."],
  [["aside", "btw"], "planner does not support Send — use BTW."],
] as const) {
  test(`a session advertising ${capabilities.join(" and ")} reads "${reason}"`, () => {
    const view = render(card({ attempts: [attempt({ delivery: "aside" })], capabilities }));
    try {
      expect(screen.getByText(reason)).toBeTruthy();
    } finally {
      view.unmount();
    }
  });
}

// LEGION-271, acceptance 7's duplicate half. An attempt that reached the listener and changed
// nothing must not read as an ordinary send, or the history claims a delivery that never
// happened.
test("a duplicate attempt says the listener already had the message", () => {
  const view = render(
    card({ attempts: [attempt({ delivery: "steer", state: "sent", duplicate: true })] })
  );
  try {
    expect(screen.getByText(duplicateText)).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("an ordinary sent attempt still reads as sent", () => {
  const view = render(card({ attempts: [attempt({ delivery: "steer", state: "sent" })] }));
  try {
    expect(screen.getByText("Sent to planner (Send)")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// LEGION-271. The stream recognises a repeated delivery for exactly its duplicate window; past
// that a same-mode retry publishes a second frame. The card promises a safe retry, and offers the
// button the promise describes, only inside that window.
test("a failed attempt inside the duplicate window promises a safe retry and offers it", () => {
  const view = render(card({ attempts: [attempt({ error: "no live session s1" })] }));
  try {
    // A cause with no trailing punctuation gets a separator, and a failure that never reached
    // the listener is not told a different mode would deliver it "again".
    expect(
      screen.getByText(
        `Failed: no live session s1. ${safeRetryGuidance("card", "no live session s1")}`
      )
    ).toBeTruthy();
    expect(screen.queryByText(/sending in a different mode/)).toBeNull();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// Only a send the listener may already have published earns the mode-change clause: that is the
// one case where another mode genuinely delivers the message a second time.
test("a receipt timeout is told what a mode change would do", () => {
  const view = render(card({ attempts: [attempt({ error: RECEIPT_TIMEOUT_CAUSE })] }));
  try {
    expect(
      screen.getByText(
        `Failed: ${RECEIPT_TIMEOUT_CAUSE} ${safeRetryGuidance("card", RECEIPT_TIMEOUT_CAUSE)}`
      )
    ).toBeTruthy();
    expect(screen.getByText(/sending in a different mode delivers it again/)).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("a failed attempt past the duplicate window makes no promise and offers no Retry", () => {
  const stale = attempt({
    createdAt: new Date(Date.now() - DELIVERY_DUPLICATE_WINDOW_MS - 60_000).toISOString(),
    error: "no live session s1",
  });
  const view = render(card({ attempts: [stale] }));
  try {
    expect(screen.getByText("Failed: no live session s1")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    // The mode-change actions stay: they are a different key and genuinely deliver.
    expect(screen.getByRole("button", { name: "Use BTW instead" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// A Retry of an attempt the session answered with an error repeats the key the stream already
// stored, so a session the listener pushes to from the stream is never handed it. The card
// offers the mode change, a new key, and names it only while that row is on screen.
test("an attempt the session answered with an error points at a mode change instead of Retry", () => {
  const answered = attempt({
    answeredWithError: true,
    delivery: "btw",
    error: "side turn failed",
  });
  const view = render(card({ attempts: [answered] }));
  try {
    expect(
      screen.getByText(`Failed: side turn failed. ${answeredWithErrorGuidance("card")}`)
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(screen.getByRole("button", { name: "Use Send instead" })).toBeTruthy();
  } finally {
    view.unmount();
  }
  // A closed issue shows no retry row, so there is no mode change to point at.
  const closed = render(card({ attempts: [answered], isClosed: true }));
  try {
    expect(screen.getByText("Failed: side turn failed")).toBeTruthy();
  } finally {
    closed.unmount();
  }
});

// A BTW recorded duplicate is still outstanding - the agent holds it and owes an answer - so the
// card must keep saying an answer is expected rather than reporting it as merely delivered.
test("a duplicate BTW still reads as waiting on an answer", () => {
  const view = render(
    card({ attempts: [attempt({ delivery: "btw", state: "sent", duplicate: true })] })
  );
  try {
    expect(screen.getByText(/Asking planner \(BTW\)/)).toBeTruthy();
  } finally {
    view.unmount();
  }
});
