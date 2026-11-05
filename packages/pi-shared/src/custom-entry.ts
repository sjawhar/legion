/**
 * The newest custom transcript entry of one type that this build can read. Each plugin state a
 * worker keeps across a `--resume` relaunch rides a `pi.appendEntry` custom entry of its own type
 * (`./phase-stall`'s `legion-phase-stall`, `./fallback-cooldown`'s `legion-model-fallback`), and
 * each of them is restored the same way: newest first, and a record this build cannot read is
 * passed over rather than read in part.
 */

/**
 * The newest `customType` entry of the branch whose data `read` answers for, or undefined when
 * the branch holds none: the walk goes newest first and stops at the first answer, so an entry
 * `read` answers undefined for — an older build's record, one with a field of the wrong type — is
 * passed over and an older readable entry behind it is still found.
 *
 * The branch is read as `readonly unknown[]` and narrowed field by field, as every reader of it
 * in this package is: a host whose entry shape differs leaves the caller with nothing rather than
 * throwing inside an event handler.
 */
export function lastCustomEntry<T>(
  branch: readonly unknown[],
  customType: string,
  read: (data: object) => T | undefined
): T | undefined {
  for (let index = branch.length - 1; index >= 0; index -= 1) {
    const entry = branch[index];
    if (
      typeof entry === "object" &&
      entry !== null &&
      "type" in entry &&
      entry.type === "custom" &&
      "customType" in entry &&
      entry.customType === customType &&
      "data" in entry &&
      typeof entry.data === "object" &&
      entry.data !== null
    ) {
      const value = read(entry.data);
      if (value !== undefined) return value;
    }
  }
  return undefined;
}
