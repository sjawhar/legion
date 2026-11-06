// The one bounded, insertion-ordered record this plugin keeps (INFRA-1026): a session remembers
// several things for its whole life — the cards it entered, the rows it read, the rows it
// claimed, the deliveries it answered — and each is a `Set` that drops its oldest entry at a
// bound of its own. The rule is written once here, so a fix to how the oldest is dropped is made
// once and no record carries a copy of it.

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
