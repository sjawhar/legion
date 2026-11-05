# ci-ok's one reading of whether `changes` selected a job and of the artifacts
# .github/actions/ci-ok-report uploads, shared by every script that reads either, so no two of
# them disagree.
#
# `selection` is read by ci-ok.sh (the verdict) and ci-ok-report.sh (whether every job that runs
# has reported). $map is SELECTED_BY (job to the `changes` output its `if:` tests), $out is
# `changes`' outputs, $pr is PULL_REQUEST_ONLY and $event is `github.event_name`. A job is
# selected when its output is 'true' and, for a PULL_REQUEST_ONLY job, the run is a pull_request
# run; `why` says which. Every script reads this file with `jq -L .github/scripts 'include
# "ci-ok"; …'`.
#
# `reports` and `posted` are the one reading of the two artifact names that action uploads, given
# the run's artifact names: the job's result, `ci-ok-<job>-<run attempt>-<success|failure|
# cancelled>`, uploaded before anything is posted, and the marker `ci-ok-posted-<job>-<run
# attempt>`, uploaded only once that job's ci-ok post succeeded. ci-ok-report.sh reads every
# job's report out of the list to tell whether it is the run's last job, and
# merge-group-report.sh reads one job's, with the marker, to tell a failure whose verdict is
# posted from one whose post never happened. The names themselves are written in
# .github/actions/ci-ok-report/action.yml, which no jq here can read while a run is going, so
# ci-ok.test.sh holds that file's two `name:` lines to these two defs: a rename that only one
# side followed would leave every follower reading `absent` and ci-ok waiting for reports it
# cannot see.
def selection($job; $map; $out; $pr; $event):
  $map[$job] as $output
  | ($out[$output] // "unset") as $value
  | ($pr | any(. == $job)) as $pr_only
  | if $value != "true" then {selected: false, why: "needs.changes.outputs.\($output) is \($value)"}
    elif $pr_only and $event != "pull_request" then
      {selected: false, why: "it runs only on pull_request, and this run is \($event)"}
    else {selected: true,
          why: "needs.changes.outputs.\($output) is true\(if $pr_only then " on a \($event)" else "" end)"}
    end;

# reports($names): every report in a list of the run's artifact names, as {job, attempt, status},
# in the list's order; a name that is no report is left out.
def reports($names):
  $names | map(capture("^ci-ok-(?<job>.+)-(?<attempt>[0-9]+)-(?<status>success|failure|cancelled)$") // empty);

# posted($names): every posted marker in that list, as {job, attempt}. A report name ends with a
# result word and a marker name with the attempt, so neither def reads the other's names.
def posted($names):
  $names | map(capture("^ci-ok-posted-(?<job>.+)-(?<attempt>[0-9]+)$") // empty);
