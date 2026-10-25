import type { ReactNode } from "react";

import { pillClassName } from "../../components/Pill";
import { TruncatedText } from "../../components/TruncatedText";
import { inputClasses, textMutedOnCanvas } from "../../theme/classes";
import type { SessionSettingsView } from "./conversation";
import type { SwitchRequest } from "./use-session-switch";

/** A header picker: a native `<select>`, as the composer's "Send as" control is. A long model id
 *  is clipped on screen only (the option's value, which a pick sends, is the whole id), so the
 *  header never runs wider than a phone. */
const selectClasses = `min-h-8 max-w-56 min-w-0 truncate rounded-lg px-2 py-1 text-xs disabled:opacity-50 ${inputClasses(false)}`;

/** The models a picker offers, in the session's order, grouped by provider in the order each
 *  provider first appears. The current model is offered even when the list lacks it (a fallback
 *  model can be one the session does not list), so the picker always shows what the session runs.
 *  An id with no provider before a `/` stands on its own, outside every group. */
function modelOptions(
  models: readonly string[],
  current: string | undefined
): { readonly loose: string[]; readonly groups: Map<string, string[]> } {
  const offered = current === undefined || models.includes(current) ? models : [...models, current];
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
 * The session's model and thinking level in the live view's header, and the pickers that switch
 * them (INFRA-195), with the line about the latest switch and the notice that says why a session
 * shows less. `view` is `sessionSettingsView`'s answer; a pick hands `onPick` what was picked, and
 * the page sends it. While a switch is pending the pickers are disabled. The values shown are the
 * session's reported ones alone: a picker snaps back to them, and moves only when the session
 * reports the switch.
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
  const thinking = view.reported ? view.settings.thinking : undefined;
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
      {thinking === undefined ? null : thinking === null ? (
        <span className={`text-xs ${textMutedOnCanvas}`} data-testid="agent-session-thinking">
          no adjustable thinking
        </span>
      ) : view.reported && view.thinkingPicker ? (
        <select
          aria-label="Thinking level"
          className={selectClasses}
          data-testid="agent-thinking-picker"
          disabled={pending}
          onChange={(event) => {
            if (event.target.value !== thinking.level) {
              onPick({ kind: "thinking", value: event.target.value });
            }
          }}
          value={thinking.level ?? ""}
        >
          {thinking.level === null ? (
            <option data-testid="agent-session-thinking" disabled value="">
              model default
            </option>
          ) : null}
          {(thinking.level === null || thinking.levels.includes(thinking.level)
            ? thinking.levels
            : [...thinking.levels, thinking.level]
          ).map((level) => choice(level, thinking.level ?? undefined, "agent-session-thinking"))}
        </select>
      ) : (
        <span
          className={pillClassName("label")}
          data-testid="agent-session-thinking"
          title="Thinking level"
        >
          {thinking.level ?? "model default"}
        </span>
      )}
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
  const { groups, loose } = modelOptions(models, current);
  return (
    <select
      aria-label="Model"
      className={selectClasses}
      data-testid="agent-model-picker"
      disabled={pending}
      onChange={(event) => {
        if (event.target.value !== current) onPick({ kind: "model", value: event.target.value });
      }}
      title={current}
      value={current ?? ""}
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
