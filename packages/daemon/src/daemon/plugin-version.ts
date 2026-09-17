import type { LegionRole } from "@legion/contracts";
import type { LegionState } from "./legion-state";
import type { ControllerLocator } from "./runtime";

interface PluginVersion {
  readonly numbers: readonly number[];
  readonly prerelease: readonly string[] | undefined;
}

function parsePluginVersion(version: string): PluginVersion | undefined {
  const [withoutBuild] = version.split("+", 1);
  const [core, prerelease] = withoutBuild?.split("-", 2) ?? [];
  if (core === undefined) return undefined;
  const numbers = core.split(".").map(Number);
  if (
    numbers.length === 0 ||
    numbers.some((part) => !Number.isSafeInteger(part) || part < 0) ||
    (prerelease !== undefined &&
      (prerelease === "" ||
        prerelease
          .split(".")
          .some((part) => !/^(?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)$/.test(part))))
  ) {
    return undefined;
  }
  return { numbers, prerelease: prerelease?.split(".") };
}

function comparePluginVersion(left: PluginVersion, right: PluginVersion): number {
  for (let index = 0; index < Math.max(left.numbers.length, right.numbers.length); index += 1) {
    const difference = (left.numbers[index] ?? 0) - (right.numbers[index] ?? 0);
    if (difference !== 0) return difference;
  }
  if (left.prerelease === undefined) return right.prerelease === undefined ? 0 : 1;
  if (right.prerelease === undefined) return -1;
  for (
    let index = 0;
    index < Math.max(left.prerelease.length, right.prerelease.length);
    index += 1
  ) {
    const leftPart = left.prerelease[index];
    const rightPart = right.prerelease[index];
    if (leftPart === undefined) return -1;
    if (rightPart === undefined) return 1;
    const leftNumeric = /^\d+$/.test(leftPart);
    const rightNumeric = /^\d+$/.test(rightPart);
    if (leftNumeric && rightNumeric) {
      const difference = Number(leftPart) - Number(rightPart);
      if (difference !== 0) return difference;
    } else if (leftNumeric !== rightNumeric) {
      return leftNumeric ? -1 : 1;
    } else if (leftPart !== rightPart) {
      return leftPart < rightPart ? -1 : 1;
    }
  }
  return 0;
}

/** An unrecorded, invalid, or older extension must be relaunched. An invalid installed manifest
 * version cannot order an otherwise-valid recorded version; the contract gate owns that invalid
 * installation. */
function pluginRequiresRelaunch(installed: string, recorded: string | undefined): boolean {
  if (recorded === undefined) return true;
  const recordedVersion = parsePluginVersion(recorded);
  if (recordedVersion === undefined) return true;
  const installedVersion = parsePluginVersion(installed);
  return (
    installedVersion !== undefined && comparePluginVersion(recordedVersion, installedVersion) < 0
  );
}

function processHandle(locator: ControllerLocator): string {
  return locator.runtime === "tmux"
    ? `pane ${locator.tmuxPaneId ?? "unrecorded"}`
    : "external" in locator
      ? `session ${locator.sessionId}`
      : `pod ${locator.podName}`;
}

function liveProcesses(
  state: LegionState
): Array<{ issue: string; role: LegionRole | "controller"; locator: ControllerLocator }> {
  return [
    ...Object.entries(state.trees).flatMap(([issue, tree]) =>
      tree.locator ? [{ issue, role: "architect" as const, locator: tree.locator }] : []
    ),
    ...Object.values(state.roles).flatMap((claim) =>
      "issue" in claim && claim.locator
        ? [{ issue: claim.issue, role: claim.role as LegionRole, locator: claim.locator }]
        : []
    ),
    ...(state.controllerLocator
      ? [{ issue: "controller", role: "controller" as const, locator: state.controllerLocator }]
      : []),
  ];
}

/** Logs every currently recorded process whose extension cannot safely be considered current. */
export function logStalePluginProcesses(
  state: LegionState,
  installedVersion: string,
  log: (line: string) => void
): void {
  for (const { issue, role, locator } of liveProcesses(state)) {
    const version = locator.pluginVersion;
    if (!pluginRequiresRelaunch(installedVersion, version)) continue;
    log(
      `[legion] live process ${issue} ${role} (${processHandle(locator)}) runs pi-legion-envoy ${version ?? "(unrecorded)"}; installed ${installedVersion} — relaunch it (LEGION-164)`
    );
  }
}
