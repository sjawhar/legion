// The bounded, insertion-ordered records this plugin keeps (INFRA-1026): a session remembers
// several things for its whole life — the rows it claimed, the deliveries it answered, the
// attempts it handled, the Dispatch message each delivered row is — and each drops its oldest
// entry at a bound of its own. Two shapes carry them, a `Set` for a record that is its values
// alone and a `Map` for one that holds a value beside its key, and the rule is written once here
// in each, so a fix to how the oldest is dropped is made once.
//
// What is not here is a queue: a list of work still in flight, where the oldest entry is dropped
// as the cap that keeps a reply nobody sent from growing for ever, not as the record of
// something seen. `user-turn-router`'s `awaited` is the one such list, and it drops its own with
// `shift()` where it is kept.

/**
 * Remember `value`, dropping the oldest entries until the record is back inside `max`.
 *
 * A `Set` iterates in insertion order, so its first entry is the one added longest ago, and
 * adding a value the set already holds leaves that order alone. The caller names `max`, which is
 * what each record's own bound says; the loop also holds a record that is already past a bound
 * to it.
 */
export function rememberBounded(remembered: Set<string>, value: string, max: number): void {
  remembered.add(value)
  while (remembered.size > max) {
    const oldest = remembered.values().next().value
    // A non-empty `Set` always yields one, and `max` is never below zero in this package; the
    // check is what keeps a bound of a different shape from looping here forever.
    if (oldest === undefined) break
    remembered.delete(oldest)
  }
}

/**
 * Remember `key` as `value`, dropping the oldest entries until the record is back inside `max`.
 *
 * The `Map`-shaped record's rule, for one that holds a value beside its key: the Dispatch
 * message each delivered row is (`./user-turn-state`). A `Map` iterates in insertion order as a
 * `Set` does, and setting a key the map already holds leaves that order alone, so the oldest
 * entry is the one its first key names.
 */
export function rememberBoundedEntry(
  remembered: Map<string, string>,
  key: string,
  value: string,
  max: number,
): void {
  remembered.set(key, value)
  while (remembered.size > max) {
    const oldest = remembered.keys().next().value
    // As above: a non-empty `Map` always yields a key, and the check is what keeps a bound of a
    // different shape from looping here forever.
    if (oldest === undefined) break
    remembered.delete(oldest)
  }
}
