import { afterEach, expect, type Mock, mock, test } from "bun:test";
import type { AgentStreamSettingsReported } from "@legion/contracts";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import type { ComponentProps } from "react";

import type { SessionSettingsView } from "./conversation";
import { SessionSettings } from "./SessionSettings";
import type { SwitchRequest } from "./use-session-switch";

type Props = ComponentProps<typeof SessionSettings>;

afterEach(cleanup);

/** The models a session lists, in its own order: two providers interleaved, and a custom
 *  provider's id holding an `@` and a trailing `:<level>`, which a pick must hand on unchanged. */
const MODELS = ["anthropic/model-a", "openai/model-c", "anthropic/model-b", "custom/model@v2:high"];

/** The view the page passes for a session whose settings frame reports model-a at thinking `auto`
 *  and that runs `/model` and `/effort` from Dispatch (INFRA-195), unless `settings` and `view`
 *  say otherwise. */
function reportedView(
  settings: Partial<Omit<AgentStreamSettingsReported, "kind" | "reported" | "seq" | "v">> = {},
  view: Partial<
    Pick<
      Extract<SessionSettingsView, { reported: true }>,
      "modelPicker" | "notice" | "thinkingPicker"
    >
  > = {}
): SessionSettingsView {
  return {
    modelPicker: true,
    notice: undefined,
    reported: true,
    settings: {
      kind: "settings",
      model: "anthropic/model-a",
      models: MODELS,
      reported: true,
      seq: 1,
      thinking: { level: "auto", levels: ["auto", "low", "high"] },
      v: 1,
      ...settings,
    },
    thinkingPicker: true,
    ...view,
  };
}

/** What a test does to the rendered header: what the pickers handed on, and new props, as the
 *  page passes them when a switch starts or ends or the session reports a new value. */
interface Header {
  onPick: Mock<(request: SwitchRequest) => void>;
  update: (props: Partial<Omit<Props, "onPick">>) => void;
}

/** The header's settings, as the page renders them: a session reporting `reportedView()` with no
 *  switch pending, unless `props` say otherwise. */
function renderSettings(props: Partial<Omit<Props, "onPick">> = {}): Header {
  const onPick = mock((_request: SwitchRequest) => {});
  let current: Props = {
    onPick,
    pending: false,
    status: undefined,
    view: reportedView(),
    ...props,
  };
  const { rerender } = render(<SessionSettings {...current} />);
  return {
    onPick,
    update: (next) => {
      current = { ...current, ...next };
      rerender(<SessionSettings {...current} />);
    },
  };
}

function picker(name: "Model" | "Thinking level"): HTMLSelectElement {
  return screen.getByRole("combobox", { name }) as HTMLSelectElement;
}

/** Whether a pending switch holds the picker: `aria-disabled`, so it keeps focus and its keys. */
function held(select: HTMLSelectElement): boolean {
  return select.getAttribute("aria-disabled") === "true";
}

/** A step from the select's own key: browsers dispatch `change` in the task of the key that
 *  stepped the closed select, which is what tells it from a pick. */
function stepWithKey(select: HTMLSelectElement, value: string): void {
  fireEvent.keyDown(select, { key: "ArrowDown" });
  fireEvent.change(select, { target: { value } });
}

// The header shows the model and thinking level the session reports now (INFRA-195), and offers
// a native picker for each where the session runs `/model` and `/effort` from Dispatch.
test("the header shows the session's reported model and thinking level, with a picker for each", () => {
  const header = renderSettings();

  const model = picker("Model");
  expect(model.value).toBe("anthropic/model-a");
  expect(screen.getByTestId("agent-session-model").textContent).toBe("anthropic/model-a");
  // One group per provider, in the order the session first lists each, models in its order.
  expect(
    [...model.querySelectorAll("optgroup")].map((group) => [
      group.label,
      [...group.querySelectorAll("option")].map((option) => option.textContent),
    ])
  ).toEqual([
    ["anthropic", ["anthropic/model-a", "anthropic/model-b"]],
    ["openai", ["openai/model-c"]],
    ["custom", ["custom/model@v2:high"]],
  ]);
  const thinking = picker("Thinking level");
  expect(thinking.value).toBe("auto");
  expect([...thinking.options].map((option) => option.textContent)).toEqual([
    "auto",
    "low",
    "high",
  ]);
  expect(screen.getByTestId("agent-session-thinking").textContent).toBe("auto");
  expect(screen.queryByTestId("agent-settings-notice")).toBeNull();
  expect(screen.queryByTestId("agent-switch-status")).toBeNull();

  // A change made anywhere else (the terminal, a fallback) shows with no turn run: here a fallback
  // model the session does not list, and a model whose thinking runs at its default.
  header.update({
    view: reportedView({
      model: "openai/fallback",
      thinking: { level: null, levels: ["low", "high"] },
    }),
  });
  expect(picker("Model").value).toBe("openai/fallback");
  expect(screen.getByTestId("agent-session-model").textContent).toBe("openai/fallback");
  expect(
    [...picker("Model").querySelectorAll('optgroup[label="openai"] option')].map(
      (option) => option.textContent
    )
  ).toEqual(["openai/model-c", "openai/fallback"]);
  expect(picker("Thinking level").value).toBe("");
  const shownLevel = screen.getByTestId("agent-session-thinking") as HTMLOptionElement;
  expect(shownLevel.textContent).toBe("model default");
  expect(shownLevel.disabled).toBe(true);
  expect(header.onPick).not.toHaveBeenCalled();
});

// A session that reports less, or cannot switch from Dispatch, says why in the header.
test("a session that cannot switch here shows what it reports, no picker, and why", () => {
  renderSettings({
    view: reportedView(
      { thinking: null },
      {
        modelPicker: false,
        notice: "This session switches its model and thinking level only at its own terminal.",
        thinkingPicker: false,
      }
    ),
  });
  expect(screen.getByTestId("agent-settings-notice").textContent).toBe(
    "This session switches its model and thinking level only at its own terminal."
  );
  expect(screen.getByTestId("agent-session-model").textContent).toBe("anthropic/model-a");
  expect(screen.getByTestId("agent-session-thinking").textContent).toBe("no adjustable thinking");
  expect(screen.queryByRole("combobox", { name: "Model" })).toBeNull();
  expect(screen.queryByRole("combobox", { name: "Thinking level" })).toBeNull();

  cleanup();
  renderSettings({
    view: {
      model: "anthropic/old-model",
      notice:
        "This session does not report its model and thinking level. Restart it on a newer pi-envoy to show and switch them here.",
      reported: false,
    },
  });
  expect(screen.getByTestId("agent-settings-notice").textContent).toBe(
    "This session does not report its model and thinking level. Restart it on a newer pi-envoy to show and switch them here."
  );
  expect(screen.getByTestId("agent-session-model").textContent).toBe("anthropic/old-model");
  expect(screen.queryByTestId("agent-session-thinking")).toBeNull();
  expect(screen.queryByRole("combobox", { name: "Model" })).toBeNull();
});

// A keyboard reader passes a model to reach the next one: the picker's arrows only move its
// choice, Enter picks once the choice reached, and leaving the picker, or the session reporting
// a new value, drops a choice not taken - the header never shows an unreported value once not
// being stepped.
test("a header picker's arrows pick nothing, Enter picks once, and leaving drops the step", async () => {
  const header = renderSettings();

  const model = picker("Model");
  stepWithKey(model, "openai/model-c");
  stepWithKey(model, "anthropic/model-b");
  expect(picker("Model").value).toBe("anthropic/model-b");
  await Bun.sleep(20);
  expect(header.onPick).not.toHaveBeenCalled();
  // The shown value stays the reported one.
  expect(screen.getByTestId("agent-session-model").textContent).toBe("anthropic/model-a");

  // Leaving the picker without Enter puts it back on the reported model.
  fireEvent.blur(picker("Model"));
  expect(picker("Model").value).toBe("anthropic/model-a");
  expect(header.onPick).not.toHaveBeenCalled();

  // A new reported value drops a step taken from the old one.
  stepWithKey(picker("Thinking level"), "low");
  expect(picker("Thinking level").value).toBe("low");
  header.update({
    view: reportedView({ thinking: { level: "high", levels: ["auto", "low", "high"] } }),
  });
  expect(picker("Thinking level").value).toBe("high");
  expect(header.onPick).not.toHaveBeenCalled();

  // Enter picks the stepped choice, once, the id exactly as the session listed it.
  stepWithKey(picker("Model"), "custom/model@v2:high");
  fireEvent.keyDown(picker("Model"), { key: "Enter" });
  expect(header.onPick).toHaveBeenCalledWith({ kind: "model", value: "custom/model@v2:high" });
  expect(header.onPick).toHaveBeenCalledTimes(1);
  // The picker shows the reported model again until the session reports the switch.
  expect(picker("Model").value).toBe("anthropic/model-a");
});

// A change no key on the select made - the pointer's, or a pick in the native popup - is a pick
// made, and picks at once.
test("a header picker's pointer change picks at once", () => {
  const header = renderSettings();

  fireEvent.change(picker("Thinking level"), { target: { value: "low" } });
  expect(header.onPick).toHaveBeenCalledWith({ kind: "thinking", value: "low" });
  expect(header.onPick).toHaveBeenCalledTimes(1);
  expect(picker("Thinking level").value).toBe("auto");
});

// While a switch waits a held picker keeps focus - `aria-disabled`, never `disabled` - but no
// pick on it picks: a pointer's pick snaps back to the reported value, and Enter on a step leaves
// the step a step until the picker is left.
test("a held picker keeps focus and its keys, and no pick on it picks", async () => {
  const header = renderSettings({ pending: true });

  expect(held(picker("Model"))).toBe(true);
  expect(held(picker("Thinking level"))).toBe(true);
  expect(picker("Model").disabled).toBe(false);
  expect(picker("Thinking level").disabled).toBe(false);

  fireEvent.change(picker("Model"), { target: { value: "openai/model-c" } });
  fireEvent.change(picker("Thinking level"), { target: { value: "low" } });
  await Bun.sleep(20);
  expect(picker("Model").value).toBe("anthropic/model-a");
  expect(picker("Thinking level").value).toBe("auto");
  stepWithKey(picker("Thinking level"), "low");
  fireEvent.keyDown(picker("Thinking level"), { key: "Enter" });
  await Bun.sleep(20);
  expect(picker("Thinking level").value).toBe("low");
  expect(header.onPick).not.toHaveBeenCalled();
  fireEvent.blur(picker("Thinking level"));
  expect(picker("Thinking level").value).toBe("auto");
  expect(header.onPick).not.toHaveBeenCalled();
  // Nothing the session has not reported shows.
  expect(picker("Model").value).toBe("anthropic/model-a");
});

// A held picker keeps focus and its keys still step, so a keyboard reader can choose while a
// switch waits; Enter then picks nothing until the switch ends, and after it picks the step once.
test("a held picker's steps survive the switch, and Enter after it ends picks the step once", async () => {
  const header = renderSettings();

  fireEvent.change(picker("Model"), { target: { value: "openai/model-c" } });
  expect(header.onPick).toHaveBeenCalledTimes(1);
  header.update({ pending: true, status: "Switching model to openai/model-c…" });
  expect(held(picker("Thinking level"))).toBe(true);

  stepWithKey(picker("Thinking level"), "low");
  stepWithKey(picker("Thinking level"), "high");
  expect(picker("Thinking level").value).toBe("high");
  fireEvent.keyDown(picker("Thinking level"), { key: "Enter" });
  await Bun.sleep(20);
  expect(header.onPick).toHaveBeenCalledTimes(1);
  expect(picker("Thinking level").value).toBe("high");
  expect(screen.getByTestId("agent-switch-status").textContent).toBe(
    "Switching model to openai/model-c…"
  );

  header.update({ pending: false, status: "The session answered: Model set to openai/model-c." });
  expect(held(picker("Thinking level"))).toBe(false);
  expect(picker("Thinking level").value).toBe("high");
  expect(header.onPick).toHaveBeenCalledTimes(1);

  fireEvent.keyDown(picker("Thinking level"), { key: "Enter" });
  expect(header.onPick).toHaveBeenLastCalledWith({ kind: "thinking", value: "high" });
  expect(header.onPick).toHaveBeenCalledTimes(2);
  expect(header.onPick.mock.calls).toEqual([
    [{ kind: "model", value: "openai/model-c" }],
    [{ kind: "thinking", value: "high" }],
  ]);
});

// A step taken while held survives the switch's end only where its picker's value did not move:
// a new value the session reported for that picker drops the step, and the other picker's stays.
test("a held picker's step gives way to a new value the session reports for that picker", () => {
  const header = renderSettings({ pending: true });

  stepWithKey(picker("Thinking level"), "low");
  stepWithKey(picker("Model"), "anthropic/model-b");
  expect(picker("Thinking level").value).toBe("low");
  expect(picker("Model").value).toBe("anthropic/model-b");

  header.update({
    pending: false,
    view: reportedView({ thinking: { level: "high", levels: ["auto", "low", "high"] } }),
  });
  expect(picker("Thinking level").value).toBe("high");
  expect(picker("Model").value).toBe("anthropic/model-b");

  fireEvent.keyDown(picker("Thinking level"), { key: "Enter" });
  expect(header.onPick).not.toHaveBeenCalled();
  fireEvent.keyDown(picker("Model"), { key: "Enter" });
  expect(header.onPick.mock.calls).toEqual([[{ kind: "model", value: "anthropic/model-b" }]]);
});
