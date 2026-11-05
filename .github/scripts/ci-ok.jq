# ci-ok's one reading of whether `changes` selected a job, shared by ci-ok.sh (the verdict) and
# ci-ok-report.sh (whether every job that runs has reported), so the two never disagree on which
# jobs run. $map is SELECTED_BY (job to the `changes` output its `if:` tests), $out is `changes`'
# outputs, $pr is PULL_REQUEST_ONLY and $event is `github.event_name`. A job is selected when its
# output is 'true' and, for a PULL_REQUEST_ONLY job, the run is a pull_request run; `why` says
# which. Both scripts read it with `jq -L .github/scripts 'include "ci-ok"; …'`.
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
