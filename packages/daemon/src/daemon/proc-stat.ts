/**
 * Field 22 (`starttime`) of a `/proc/<pid>/stat` line: the process's start time in clock ticks
 * since boot. Stable for the process's whole life, so together with the pid it is the identity a
 * pane locator records at launch and re-checks before trusting (or killing) that pane. Field 2
 * (`comm`) is the executable name in parentheses and may itself contain spaces or parentheses
 * (`(tmux: server)`), so fields are counted only after the LAST `)`: what follows it is field 3
 * onward, which makes `starttime` the 20th whitespace-separated token there. Throws on a
 * malformed line -- a guessed value here could let a stale pane verify as ours.
 */
export function parseProcStatStartTicks(stat: string): number {
  const commEnd = stat.lastIndexOf(")");
  if (commEnd === -1) {
    throw new Error(`malformed /proc/<pid>/stat line (no comm terminator): ${stat.trim()}`);
  }
  const fields = stat
    .slice(commEnd + 1)
    .trim()
    .split(/\s+/);
  const token = fields[22 - 3];
  if (token === undefined || !/^\d+$/.test(token)) {
    throw new Error(`malformed /proc/<pid>/stat line (no starttime field): ${stat.trim()}`);
  }
  const ticks = Number(token);
  if (!Number.isSafeInteger(ticks)) {
    throw new Error(`/proc/<pid>/stat starttime out of range: ${token}`);
  }
  return ticks;
}
