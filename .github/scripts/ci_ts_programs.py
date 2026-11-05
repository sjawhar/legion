"""The pairs check-ts-filters.sh derives the TypeScript half of each Bun gate's reads from: a
filter of ci.yaml's changes job and the tsc project the job that filter gates typechecks. The
script's header says what it proves, which gates it leaves out and why.

Each entry is (filter, project, why):
  filter   a filter of the changes job, the gate of the job whose typecheck this program is
  project  what `bunx tsc -p` takes: the directory holding the tsconfig.json the job's typecheck
           reads, or the project file itself where the job's step names another one
  why      the ci.yaml step the pair stands for, read from that job's own steps, which the
           failure message carries so the reader knows which typecheck holds the file

The pairs are hand-kept and few; the files of each program are derived, which is the point. A
job that typechecks a second project is a second entry (the envoy-client job typechecks
broker-client beside its own package, the dispatch-ui job the proof editor beside the dashboard,
and the test job the three script directories that are no workspace).
"""

PROGRAMS = [
    ("contracts", "packages/contracts", "contracts: Typecheck contracts (bun run typecheck: bunx tsc --noEmit)"),
    ("envoy_client", "packages/envoy-client", "envoy-client: Typecheck Envoy client"),
    ("envoy_client", "packages/broker-client", "envoy-client: Typecheck broker client"),
    ("pi_shared", "packages/pi-shared", "pi-shared: Typecheck pi-shared"),
    ("pi_envoy", "packages/pi-envoy", "pi-envoy: Typecheck pi-envoy"),
    ("pi_legion", "packages/pi-legion", "pi-legion: Typecheck pi-legion"),
    ("claude_envoy", "packages/claude-envoy", "claude-envoy: Typecheck Claude Envoy plugin"),
    ("dispatch_ui", "packages/dispatch", "dispatch-ui: Typecheck dispatch dashboard"),
    # The proof editor's and the docs site's steps run the same `bun run typecheck`, whose recipe
    # names a project file of its own rather than the directory's tsconfig.json.
    ("dispatch_ui", "packages/proof-editor/tsconfig.check.json",
     "dispatch-ui: Typecheck proof editor (bun run typecheck: bunx tsc --noEmit -p tsconfig.check.json)"),
    ("docs", "docs/site/tsconfig.tools.json",
     "docs-site.yaml's build job: Typecheck the docs site (bun run typecheck: tsc --noEmit -p tsconfig.tools.json), "
     "which ci.yaml's docs job calls"),
    ("conflicts_script", "scripts/conflicts", "conflicts-script: Typecheck the conflict measurement script"),
    ("test", "scripts/dispatch", "test: Typecheck the Dispatch census script (bunx tsc --noEmit -p scripts/dispatch)"),
    ("test", "scripts/github-apps",
     "test: Typecheck the GitHub App registration script (bunx tsc --noEmit -p scripts/github-apps)"),
    ("test", "scripts/sessions",
     "test: Typecheck the session message-wait script (bunx tsc --noEmit -p scripts/sessions)"),
]
