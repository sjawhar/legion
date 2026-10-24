# The pairing of every undelivered person's spec decision with what became of it, read from a
# Legion daemon's JSON log: `legion start`'s stderr, or a log store's entries reduced to the
# daemon's own objects. Input: the log, raw (jq -R -s); a line that is not a JSON object is skipped.
# Output: {undelivered, sent, unrelayed, unmatched}: each `outbox notice finished undelivered: its
# architect …` line whose kind is a spec decision; each line that relayed one or found it relayed;
# each line that relayed nothing because no person made it, with the notice's ref (empty on a
# notice the release before queued); and each undelivered line none of those names by its row.
def spec_decision: IN("design-approved", "design-changes-requested", "carried-gate-opened");
def relayed: IN("outbox parked decision sent to the controller", "outbox parked decision already sent to the controller");
def unrelayed: . == "outbox parked decision not relayed: it names no person";
[split("\n")[] | fromjson? | select(type == "object" and (.msg | type) == "string")] as $log
| [$log[] | select((.msg | startswith("outbox notice finished undelivered: its architect")) and (.kind | spec_decision))] as $undelivered
| [$log[] | select(.msg | relayed)] as $sent
| [$log[] | select(.msg | unrelayed)] as $unrelayed
| {
    undelivered: [$undelivered[] | {time, row, kind, issue}],
    sent: [$sent[] | {time, msg, row, key, kind, issue, root}],
    unrelayed: [$unrelayed[] | {time, row, key, kind, issue, ref}],
    unmatched: [$undelivered[] | . as $u | select(any($sent[], $unrelayed[]; .row == $u.row) | not) | {time, row, kind, issue}]
  }
