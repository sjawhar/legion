# The pairing of every undelivered person's spec decision with what became of it, read from a
# Legion daemon's JSON log: `legion start`'s stderr, or a log store's entries reduced to the
# daemon's own objects. Input: the log, raw (jq -R -s); a line that is not a JSON object is skipped.
# Output: {undelivered, sent, kept, handled, unrelayed, unmatched}: each `outbox notice finished
# undelivered: its architect …` line whose kind is a spec decision; each line that relayed one to
# the controller; each line that kept one for the start of a root that waits for its slot; each
# line of a row whose dedupe key an earlier run or another row carrying the same notice already
# handled (its own line says what became of the decision, and it is never counted as sent); each
# line that relayed nothing, with the notice's ref (empty on a notice the release before queued,
# absent where the root is no parked tree); and each undelivered line none of those names by its
# row.
def spec_decision: IN("design-approved", "design-changes-requested", "carried-gate-opened");
def relayed: . == "outbox parked decision sent to the controller";
def kept: . == "outbox parked decision kept for its root's start, which waits for its slot";
def handled: . == "outbox parked decision handled before under this key";
def unrelayed: startswith("outbox parked decision not relayed:");
[split("\n")[] | fromjson? | select(type == "object" and (.msg | type) == "string")] as $log
| [$log[] | select((.msg | startswith("outbox notice finished undelivered: its architect")) and (.kind | spec_decision))] as $undelivered
| [$log[] | select(.msg | relayed)] as $sent
| [$log[] | select(.msg | kept)] as $kept
| [$log[] | select(.msg | handled)] as $handled
| [$log[] | select(.msg | unrelayed)] as $unrelayed
| {
    undelivered: [$undelivered[] | {time, row, kind, issue}],
    sent: [$sent[] | {time, msg, row, key, kind, issue, root}],
    kept: [$kept[] | {time, row, key, kind, issue, root}],
    handled: [$handled[] | {time, msg, row, key, kind, issue}],
    unrelayed: [$unrelayed[] | {time, msg, row, key, kind, issue, ref}],
    unmatched: [$undelivered[] | . as $u | select(any($sent[], $kept[], $handled[], $unrelayed[]; .row == $u.row) | not) | {time, row, kind, issue}]
  }
