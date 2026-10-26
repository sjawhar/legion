import type { AgentStreamSettingsReported } from "@legion/contracts";
import {
  type ChangeEvent,
  type KeyboardEvent,
  type ReactNode,
  useMemo,
  useRef,
  useState,
} from "react";

import { pillClassName } from "../../components/Pill";
import { TruncatedText } from "../../components/TruncatedText";
import { inputClasses, textMutedOnCanvas } from "../../theme/classes";
import type { SessionSettingsView } from "./conversation";
import type { SwitchRequest } from "./use-session-switch";

/** A header picker: a native `<select>`, as the composer's "Send as" control is. A long model id
 *  is clipped on screen only (the option's value, which a pick sends, is the whole id), so the
 *  header never runs wider than a phone. A held picker (`aria-disabled`, `useKeySteps`) looks
 *  disabled through the `refused:` variant (`styles.css`). */
const selectClasses = `min-h-8 max-w-56 min-w-0 truncate rounded-lg px-2 py-1 text-xs refused:opacity-50 ${inputClasses(false)}`;

/** The values a picker offers: the session's list, with the current value added when the list
 *  lacks it (a fallback model can be one the session does not list), so the picker always shows
 *  what the session runs. */
function withCurrent(list: readonly string[], current: string | undefined): readonly string[] {
  return current === undefined || list.includes(current) ? list : [...list, current];
}

/** The models a picker offers (`withCurrent`), in the session's order, grouped by provider in the
 *  order each provider first appears. An id with no provider before a `/` stands on its own,
 *  outside every group. */
function modelOptions(
  models: readonly string[],
  current: string | undefined
): { readonly loose: string[]; readonly groups: Map<string, string[]> } {
  const offered = withCurrent(models, current);
  const loose: string[] = [];
  const groups = new Map<string, string[]>();
  for (const id of offered) {
    const slash = id.indexOf("/");
    if (slash <= 0) {
      loose.push(id);
      continue;
    }
    const provider = id.slice(0, slash);
    const group = groups.get(provider);
    if (group === undefined) groups.set(provider, [id]);
    else group.push(id);
  }
  return { groups, loose };
}

/** One option of a picker; the selected one carries the shown value's test id. */
function choice(value: string, selected: string | undefined, testId: string): ReactNode {
  return (
    <option data-testid={value === selected ? testId : undefined} key={value} value={value}>
      {value}
    </option>
  );
}

/**
 * A header picker's choice: its change switches the live session, so the rule is the one the
 * Agents issue picker keeps (`markKeyStep` in `AgentMessageComposer.tsx`). The select's own keys
 * only move its choice, so a keyboard reader can pass one model to reach the next; `Enter` picks
 * the choice reached (and stops there); a pick made with the pointer or in the native popup picks
 * at once; and leaving the select without picking, or the session reporting a new value, puts it
 * back on `reported`. Once not being stepped the select shows the reported value alone, a pick
 * included: the header moves only when the session reports the switch.
 *
 * While `held` (a switch is pending) the select is `aria-disabled` rather than `disabled`, so it
 * keeps focus and its keys still step its choice, but nothing picks: `Enter` and a pointer's or
 * popup's pick send nothing, and the controlled select snaps back to the value it shows. A step
 * taken while held survives the switch's end, so `Enter` then picks it.
 *
 * Whether a change is a key's step is told by the task it arrives in, not the key: every engine
 * steps a closed select from the key event's own default action (the arrows, `Home`/`End` and the
 * page keys from `keydown`, type-ahead from `keypress`) and dispatches `change` in that same task,
 * while a pick made in a native popup a key opened arrives in a later task, as a pointer's does.
 * So each key on the select marks the step and the next task clears the mark.
 */
function useKeySteps(
  reported: string,
  held: boolean,
  pick: (value: string) => void
): {
  "aria-disabled": true | undefined;
  onBlur: () => void;
  onChange: (event: ChangeEvent<HTMLSelectElement>) => void;
  onKeyDown: (event: KeyboardEvent<HTMLSelectElement>) => void;
  onKeyPress: () => void;
  value: string;
} {
  /** The reader's step, with the reported value it was taken from: a step from an older value is
   *  dropped, so a new reported value resets the choice. */
  const [step, setStep] = useState<{ from: string; value: string }>();
  const movedByKeyboard = useRef(false);
  const markKeyStep = () => {
    movedByKeyboard.current = true;
    setTimeout(() => {
      movedByKeyboard.current = false;
    }, 0);
  };
  const take = (value: string) => {
    if (held) return;
    setStep(undefined);
    if (value !== reported) pick(value);
  };
  return {
    "aria-disabled": held || undefined,
    onBlur: () => setStep(undefined),
    onChange: (event) => {
      if (movedByKeyboard.current) setStep({ from: reported, value: event.target.value });
      else take(event.target.value);
    },
    onKeyDown: (event) => {
      if (event.key !== "Enter") {
        markKeyStep();
        return;
      }
      event.preventDefault();
      take(event.currentTarget.value);
    },
    onKeyPress: markKeyStep,
    value: step !== undefined && step.from === reported ? step.value : reported,
  };
}

/**
 * The session's model and thinking level in the live view's header, and the pickers that switch
 * them (INFRA-195), with the line about the latest switch and the notice that says why a session
 * shows less. `view` is `sessionSettingsView`'s answer; a pick hands `onPick` what was picked, and
 * the page sends it. While a switch is pending the pickers are held (`aria-disabled`): they keep
 * focus and their keys still step, but nothing picks until the switch ends. A picker's own keys
 * only step its choice and `Enter` picks it (`useKeySteps`); the values shown, once not being
 * stepped, are the session's reported ones alone: a picker snaps back to them, and moves only when
 * the session reports the switch.
 *
 * The status and the notice sit on lines of their own at the header's end (`order-last`).
 */
export function SessionSettings({
  onPick,
  pending,
  status,
  view,
}: {
  onPick: (request: SwitchRequest) => void;
  pending: boolean;
  status: string | undefined;
  view: SessionSettingsView;
}): ReactNode {
  const model = view.reported ? view.settings.model : view.model;
  return (
    <>
      {view.reported && view.modelPicker ? (
        <ModelPicker
          current={model}
          models={view.settings.models}
          onPick={onPick}
          pending={pending}
        />
      ) : model === undefined ? null : (
        <span className={`${pillClassName("label")} max-w-56`} data-testid="agent-session-model">
          <TruncatedText className="min-w-0" title={model}>
            {model}
          </TruncatedText>
        </span>
      )}
      {view.reported ? (
        <ThinkingControl
          offered={view.thinkingPicker}
          onPick={onPick}
          pending={pending}
          thinking={view.settings.thinking}
        />
      ) : null}
      {status === undefined ? null : (
        <p
          className={`order-last basis-full text-xs break-words whitespace-pre-wrap ${textMutedOnCanvas}`}
          data-testid="agent-switch-status"
          role="status"
        >
          {status}
        </p>
      )}
      {view.notice === undefined ? null : (
        <p
          className={`order-last basis-full text-xs ${textMutedOnCanvas}`}
          data-testid="agent-settings-notice"
        >
          {view.notice}
        </p>
      )}
    </>
  );
}

function ModelPicker({
  current,
  models,
  onPick,
  pending,
}: {
  current: string | undefined;
  models: readonly string[];
  onPick: (request: SwitchRequest) => void;
  pending: boolean;
}): ReactNode {
  const { groups, loose } = useMemo(() => modelOptions(models, current), [models, current]);
  const keySteps = useKeySteps(current ?? "", pending, (value) => onPick({ kind: "model", value }));
  return (
    <select
      aria-label="Model"
      className={selectClasses}
      data-testid="agent-model-picker"
      {...keySteps}
      title={current}
    >
      {current === undefined ? (
        <option disabled value="">
          no model selected
        </option>
      ) : null}
      {loose.map((id) => choice(id, current, "agent-session-model"))}
      {[...groups].map(([provider, ids]) => (
        <optgroup key={provider} label={provider}>
          {ids.map((id) => choice(id, current, "agent-session-model"))}
        </optgroup>
      ))}
    </select>
  );
}

/** The session's thinking level: `no adjustable thinking` when the model has none, a pill where
 *  the session switches it only at its own terminal, and otherwise the picker. */
function ThinkingControl({
  offered,
  onPick,
  pending,
  thinking,
}: {
  offered: boolean;
  onPick: (request: SwitchRequest) => void;
  pending: boolean;
  thinking: AgentStreamSettingsReported["thinking"];
}): ReactNode {
  const keySteps = useKeySteps(thinking?.level ?? "", pending, (value) =>
    onPick({ kind: "thinking", value })
  );
  if (thinking === null) {
    return (
      <span className={`text-xs ${textMutedOnCanvas}`} data-testid="agent-session-thinking">
        no adjustable thinking
      </span>
    );
  }
  if (!offered) {
    return (
      <span
        className={pillClassName("label")}
        data-testid="agent-session-thinking"
        title="Thinking level"
      >
        {thinking.level ?? "model default"}
      </span>
    );
  }
  const current = thinking.level ?? undefined;
  return (
    <select
      aria-label="Thinking level"
      className={selectClasses}
      data-testid="agent-thinking-picker"
      {...keySteps}
    >
      {thinking.level === null ? (
        <option data-testid="agent-session-thinking" disabled value="">
          model default
        </option>
      ) : null}
      {withCurrent(thinking.levels, current).map((level) =>
        choice(level, current, "agent-session-thinking")
      )}
    </select>
  );
}
