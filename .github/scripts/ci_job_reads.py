"""What each ci.yaml job reads that check-ci-filters.sh cannot find by itself: the data it checks
the changes job's filters against (ci_filter_coverage.py is the engine; check-ci-filters.sh's
header says what is proved).

  GATES     one Gate per filter: the job (or step) it gates, the go commands the job runs, and
            the files its other steps read. Each go command is pinned to the text that starts it:
            `step=` a step of the job whose run: holds `text`, or `source=` a script that holds it.
            A step whose go commands are not in the simple form is an `exact=` entry instead
            (pinned_step): its whole run: script and directory, and the Runs that stand for what
            it compiles. The check fails when a run: step of a ci.yaml job starts a go command no
            Run or exact entry claims, and when a pin's text is gone, so the table cannot drift
            from ci.yaml silently. A claimed command the check does not reduce (check-ci-filters.sh's
            header says which it does), and every Run of an exact entry whose step no longer holds
            its script, counts as reading every Go file of the repository and every file under a
            Go module's directory; a script's pin is its whole line.
  RULES     a source file -> what it does when a test run compiles it: a go command it starts (a
            Run, whose closure is added) or a file it reads outside its package (a Read). A
            _test.go file counts when go list lists it in a tested package under the job's tags;
            any other file when it is compiled into a test run's closure. These are found by
            reading the tests, not by the check: a new read through `..` or a new go command a
            test starts is added here by the change that makes it.
  EXCLUDED  filters the check leaves to another: worker_image gates worker-image.yaml's image
            build, whose every input .github/scripts/check-image-trigger-paths.sh proves its
            filter covers. (envoy_smoke's image inputs are that check's too, but its job's go
            commands and other step reads are a gate here.)
  PUSH_WORKFLOWS  filter -> a workflow that runs that filter's job's work on a push to main in a
            workflow of its own, selected by its `on.push.paths`, which must equal the filter: the
            filter decides the pull request's and merge group's run, the paths main's.

A Read's glob is expanded over the repository's files; a why cites the step (at()) or the
source line that reads it. at()'s third argument is the workflow file that holds the step, and
it is ci.yaml unless a job gives its steps away: ci.yaml's docs job calls docs-site.yaml, so the
docs gate cites that file's build job (at_docs). The Read lists of the jobs without Go steps
(the Bun packages) are as hand-kept as RULES: the check proves the filter covers them, not that
they are complete.
"""

import json
from pathlib import Path

from ci_filter_coverage import Read, at, exact, gate, run

ENVOY = "packages/envoy"
DAEMON = "packages/daemon"
INFRA = "packages/envoy/infra"
FIX = "packages/contracts/fixtures"
GO_TARGETS = ".github/go-release-targets.json"
CLIENT_TARGETS = ".github/agent-secrets-client-targets.json"
INFRA_TARGETS = ".github/envoy-infra-targets.json"
NOTICES = "scripts/go-third-party-notices.sh"
CI = ".github/workflows/ci.yaml"
DOCS_SITE = ".github/workflows/docs-site.yaml"


def targets(path: str, key: str) -> list:
    return json.loads(Path(path).read_text())[key]


def step_run(job, step, text, workdir, pkgs, **options):
    """A go command a ci.yaml step starts, pinned to the step and its text."""
    return run(workdir, pkgs, f"{at(job, step)}: {text}", step=step, text=text, **options)


def script_run(source, text, started_by, workdir, pkgs, **options):
    """A go command a script starts, pinned to the script and its text."""
    return run(workdir, pkgs, f"{source} ({started_by}): {text}", source=source, text=text, **options)


def pinned_step(job, step, workdir, script, runs):
    """A ci.yaml step pinned to its whole run: script and the directory it runs in (an Exact
    entry). Each of `runs`, (workdir, packages, what the script compiles there, run options),
    stands for a go command of the script; together they must name all it compiles."""
    return exact(step, workdir, script, [run(where, pkgs, f"{at(job, step)} (its pinned script): {what}", **options)
                                         for where, pkgs, what, options in runs])


def reads(*entries):
    return [Read(glob, why) for glob, why in entries]


def envoy_top_level_but_infra():
    """packages/envoy/* and every top-level directory of it but infra, node_modules and testdata."""
    globs = [f"{ENVOY}/*"]
    for entry in sorted(Path(ENVOY).iterdir()):
        if entry.is_dir() and entry.name not in {"infra", "node_modules", "testdata"}:
            globs.append(f"{ENVOY}/{entry.name}/**")
    return globs


# ---------------------------------------------------------------- shared step reads
WORKSPACES = [
    "packages/contracts", "packages/envoy-client", "packages/pi-envoy", "packages/pi-legion",
    "packages/pi-shared", "packages/claude-envoy", "packages/proof-editor",
    "packages/dispatch", "packages/envoy/internal/dispatch/pmdoc/gen", "docs/site",
]


def bun_job(job, workflow=CI):
    """.github/actions/setup-bun and a frozen `bun install`. No bunfig.toml at the root and no
    .npmrc anywhere; packages/dispatch/bunfig.toml is read only by bun test there."""
    install = at(job, "bun install --frozen-lockfile", workflow)
    return reads(
        (".github/actions/setup-bun/**", at(job, "./.github/actions/setup-bun", workflow)),
        (".bun-version", ".github/actions/setup-bun/action.yml:22 bun-version-file: .bun-version"),
        ("package.json", f"{install} reads the root manifest (workspaces package.json:19-31, patchedDependencies :45-49)"),
        ("bun.lock", f"{install} refuses a lock that disagrees with the manifests"),
        ("patches/**", f"{install} applies package.json:45-49 patchedDependencies"),
        *[(f"{w}/package.json", f"{install}: package.json:19-31 lists {w}; a frozen install reads every workspace manifest")
          for w in WORKSPACES],
    )


def biome(where):
    return reads(
        ("biome.json", f"{where}: Biome reads the root config"),
        (".gitignore", f"{where}: biome.json:3-6 vcs.useIgnoreFile reads the root .gitignore"),
    )


def install_jj(job):
    return reads(
        (".github/actions/install-jj/**", at(job, "./.github/actions/install-jj")),
        (f"{DAEMON}/docker/worker.Dockerfile", ".github/actions/install-jj/action.yml:22-24,49-50 read ARG MISE_VERSION, MISE_SHA256, JJ_TOOL"),
    )


def install_omp(job):
    return reads(
        (".github/actions/install-omp/**", at(job, "./.github/actions/install-omp")),
        (".omp-pin", ".github/actions/install-omp/action.yml:16 pin=$(<.omp-pin)"),
    )


def setup_go(job, gomod, workflow=CI):
    return reads((gomod, f"{at(job, 'actions/setup-go', workflow)} go-version-file"))


CI_SELF = reads((CI, "a job reads its own definition"))
# ci.yaml's docs job and docs-pages.yaml's build job both call docs-site.yaml, which holds the
# steps. The gate stays on ci.yaml's docs job, whose if: the docs filter gates, and every step
# citation names docs-site.yaml's build job.
DOCS_JOB = "build"
DOCS_SITE_SELF = reads((DOCS_SITE, "the docs build reads the definition of the steps it runs"))


def at_docs(step):
    return at(DOCS_JOB, step, DOCS_SITE)


CONTRACTS_SRC = "packages/contracts/src/**"
ENVOY_CLIENT_SRC = "packages/envoy-client/src/**"

# What packages/claude-envoy/scripts/verify-build.sh and the claude-envoy job's other steps read:
# the claude-envoy gate, and the test job's verify-build step.
CLAUDE_ENVOY_BUILD = reads(
        ("packages/claude-envoy/**", f"{at('claude-envoy', 'Lint Claude Envoy plugin')}, typecheck, verify-build, test and pack in packages/claude-envoy"),
        (CONTRACTS_SRC, "packages/claude-envoy/tsconfig.json paths @legion/contracts; bundles import it"),
        (ENVOY_CLIENT_SRC, "packages/claude-envoy/src/envoy-channel-server.ts:9-62, hooks/session-hook.ts:41-48 import @legion/envoy-client/*"),
        ("packages/envoy-client/bin/dispatch.ts", "packages/claude-envoy/scripts/build.ts's BUNDLE_ENTRYPOINTS dispatch (verify-build, tests/dist-fresh.test.ts:303)"),
        ("packages/pi-shared/src/**", "packages/claude-envoy/src/envoy-channel-server.ts:63-64, legion-claim.ts:7-9, legion-tool.ts:3-11 import @legion/pi-shared/*"),
        (f"{FIX}/daemon-api/**", "packages/claude-envoy/tests/daemon-api-version.test.ts:15, legion-claim.test.ts:30, legion-pane-mcp.test.ts:17, legion-tool.test.ts:12"),
        ("scripts/third-party-notices.ts", "packages/claude-envoy/scripts/build.ts:20"),
        ("scripts/pi-plugin-prepack.sh", "packages/claude-envoy/scripts/build.ts:173-179 --stage-skills @sjawhar/pi-legion"),
        ("packages/pi-legion/agents/**", "packages/claude-envoy/scripts/build.ts:183-187"),
        ("skills/**", "packages/claude-envoy/scripts/build.ts:173-179 (staged pi-legion partition); packages/claude-envoy/skills/* symlinks into ../../../skills"),
        (".bun-version", "packages/claude-envoy/scripts/build.ts:28 reads the Bun pin"),
        (".claude-plugin/marketplace.json", "packages/claude-envoy/tests/envoy-channel-server.test.ts:7"),
        (".github/scripts/check-private-names.sh", "packages/claude-envoy/scripts/verify-build.sh checks the build with it"),
        (".github/scripts/release-push.sh", "packages/claude-envoy/scripts/verify-build.sh runs its check-built; the job runs its extract-built"),
)


# ---------------------------------------------------------------- RULES
LEGION_BUILD = "./cmd/legion"
PACK_READS = [  # bun pm pack of pi-envoy / pi-legion runs their prepack (scripts/pi-plugin-prepack.sh)
    "packages/pi-envoy/**", "packages/pi-legion/**", "packages/pi-shared/**", "packages/envoy-client/**",
    "packages/contracts/**", "scripts/pi-plugin-prepack.sh", "scripts/third-party-notices.ts", "skills/**",
    "package.json", "bun.lock", "patches/**",
]
PMDOC_GEN = [f"{ENVOY}/internal/dispatch/pmdoc/gen/**", "packages/proof-editor/**", "package.json", "bun.lock", "patches/**"]
BROKER_REFGEN_DIRS = ("internal/broker/api", "internal/broker/helper", "internal/broker/config", "cmd/broker", "cmd/agent-secrets")
MODULE_SOURCES = [Read(f"{ENVOY}/**", "internal/dispatch/api/lock_level_test.go's moduleSources walks every non-test .go file "
                                      "under packages/envoy (infra included); used by :75, event_insert_guard_test.go:32, "
                                      "review_write_guard_test.go:44")]


def legion_build(why):
    return run(DAEMON, [LEGION_BUILD], why, mode="build")


RULES = {
    # --- daemon
    f"{DAEMON}/cmd/legion/config_example_test.go": reads(("deploy/kubernetes/daemon/legion.yaml.example", "cmd/legion/config_example_test.go:24 (runtime.Caller + ../../../..)")),
    f"{DAEMON}/cmd/legion/gh_test.go": reads((f"{DAEMON}/docker/gh", "cmd/legion/gh_test.go:880 ../../docker/gh")),
    f"{DAEMON}/internal/admit/admit_test.go": reads((f"{DAEMON}/internal/intake/testdata/dispatch/issue-updated.json", "internal/admit/admit_test.go:993")),
    f"{DAEMON}/internal/api/real_github_test.go": reads(("scripts/e2e/lib/github-apps.sh", "internal/api/real_github_test.go:88,107 sources it in bash")),
    f"{DAEMON}/internal/api/state_golden_test.go": reads((f"{FIX}/daemon-api/*", "internal/api/state_golden_test.go:27")),
    f"{DAEMON}/internal/capabilities/capabilities_test.go": reads(
        (f"{FIX}/capability-redaction.json", "internal/capabilities/capabilities_test.go:236 readFixture"),
        (f"{FIX}/capability-redaction-plugin-report.json", "internal/capabilities/capabilities_test.go:236 readFixture")),
    f"{DAEMON}/internal/claim/claim_test.go": reads((f"{FIX}/project-tokens.json", "internal/claim/claim_test.go:196")),
    f"{DAEMON}/internal/classify/fixtures_test.go": reads((f"{FIX}/classification/**", "internal/classify/fixtures_test.go:179 (runtime.Caller :184)")),
    f"{DAEMON}/internal/config/controller_test.go": reads(("deploy/kubernetes/daemon/controller.yaml.example", "internal/config/controller_test.go:179 (config.ControllerConfigExample)")),
    f"{DAEMON}/internal/config/kubernetes_test.go": reads(("deploy/kubernetes/operator-route/pod.yml", "internal/config/kubernetes_test.go:1322")),
    f"{DAEMON}/internal/daemon/agentmodels_omp_test.go": reads(
        ("packages/pi-legion/agents/**", "internal/daemon/agentmodels_omp_test.go:165 copyDir"),
        ("scripts/pi-plugin-prepack.sh", "internal/daemon/agentmodels_omp_test.go:175 --stage-skills"),
        ("skills/**", "internal/daemon/agentmodels_omp_test.go:175 prepack stages skills/<name>")),
    f"{DAEMON}/internal/daemon/capability_reports_test.go": reads((f"{FIX}/capability-redaction.json", "internal/daemon/capability_reports_test.go:315")),
    f"{DAEMON}/internal/daemon/check_settlement_test.go": reads((f"{FIX}/checks-settlement/*", "internal/daemon/check_settlement_test.go:56")),
    f"{DAEMON}/internal/daemon/daemon_test.go": reads((f"{DAEMON}/internal/intake/testdata/dispatch/issue-updated.json", "internal/daemon/daemon_test.go:1120")),
    f"{DAEMON}/internal/daemon/nats_permissions_test.go": reads(
        (f"{DAEMON}/internal/intake/testdata/dispatch/issue-updated.json", "internal/daemon/nats_permissions_test.go:76"),
        (f"{DAEMON}/internal/intake/testdata/github/pr-opened.json", "internal/daemon/nats_permissions_test.go:77")),
    f"{DAEMON}/internal/daemon/outbox_scratch_dispatch_test.go": [
        run(ENVOY, ["./cmd/dispatch"], "internal/daemon/outbox_scratch_dispatch_test.go:187 go build ./cmd/dispatch in packages/envoy", mode="build")],
    f"{DAEMON}/internal/daemon/promptreferences_omp_test.go": [
        Read(g, "internal/daemon/promptreferences_omp_test.go:132,207-228 bun pm pack of packages/pi-envoy and pi-legion runs their prepack")
        for g in PACK_READS] + reads(("packages/envoy-client/scripts/build-dispatch-cli.sh", "scripts/pi-plugin-prepack.sh:92, run by the pack in promptreferences_omp_test.go:228")),
    f"{DAEMON}/internal/daemon/supervision_test.go": reads((f"{DAEMON}/internal/stream/*", "internal/daemon/supervision_test.go:1220 Glob ../stream/*.go")),
    f"{DAEMON}/internal/intake/redelivered_test.go": [
        run(ENVOY, ["./cmd/listener", "./cmd/dispatch"], "internal/intake/redelivered_test.go:255 go build -C ../../../envoy ./cmd/listener ./cmd/dispatch", mode="build")],
    f"{DAEMON}/internal/intake/intake_test.go": reads((f"{FIX}/github-envelopes/*", "internal/intake/intake_test.go:782,855")),
    f"{DAEMON}/internal/projection/state_golden_test.go": reads((f"{FIX}/daemon-api/*", "internal/projection/state_golden_test.go:161")),
    f"{DAEMON}/internal/prompts/roles_test.go": reads(("packages/pi-legion/agents/*", "internal/prompts/roles_test.go:236")),
    f"{DAEMON}/internal/runtime/sandbox/operatorpod_test.go": reads(
        ("deploy/kubernetes/operator-route/pod.yml", "internal/runtime/sandbox/operatorpod_test.go:25"),
        (f"{FIX}/dispatch-public-url.json", "internal/runtime/sandbox/operatorpod_test.go:225"),
        (f"{DAEMON}/docker/worker.Dockerfile", "internal/runtime/sandbox/operatorpod_test.go:287")),
    f"{DAEMON}/internal/runtime/sandbox/reclaim_doc_test.go": reads(("docs/kubernetes.md", "internal/runtime/sandbox/reclaim_doc_test.go:144")),
    f"{DAEMON}/internal/runtime/sandbox/boundary_test.go": [legion_build("internal/runtime/sandbox/boundary_test.go:643 go build .../daemon/cmd/legion")],
    f"{DAEMON}/internal/runtime/tmux/tmux_real_test.go": [
        legion_build("internal/runtime/tmux/tmux_real_test.go:74 go build .../daemon/cmd/legion"),
        Read(f"{DAEMON}/cmd/legion/testdata/gh-help/**", "internal/runtime/tmux/tmux_real_test.go:1822")],
    f"{DAEMON}/internal/stream/integration_test.go": [legion_build("internal/stream/integration_test.go:130 go build .../daemon/cmd/legion")],
    f"{DAEMON}/internal/runtime/workerbin/workerbin_test.go": reads((f"{DAEMON}/docker/gh", "internal/runtime/workerbin/workerbin_test.go:58")),
    # --- envoy
    f"{ENVOY}/cmd/broker-refgen/auth_test.go": [
        Read(f"{ENVOY}/{d}/*", "cmd/broker-refgen/auth_test.go:14 readAPI(repo root) parses it") for d in BROKER_REFGEN_DIRS],
    f"{ENVOY}/cmd/broker/main_test.go": reads((f"{ENVOY}/docker/rds-global-bundle.pem", "cmd/broker/main_test.go:220 sslrootcert=../../docker/rds-global-bundle.pem")),
    f"{ENVOY}/cmd/broker/outage_test.go": [
        run(ENVOY, ["./cmd/agent-secrets"], "cmd/broker/outage_test.go:317-318 go build . in ../agent-secrets", mode="build")],
    f"{ENVOY}/cmd/broker/multiprocess_test.go": reads((f"{ENVOY}/internal/broker/store/migrations/*", "cmd/broker/multiprocess_test.go:289")),
    f"{ENVOY}/cmd/dispatch/main_test.go": reads((f"{ENVOY}/internal/dispatch/store/migrations/*", "cmd/dispatch/main_test.go:695")),
    # cmd/dispatch/memory_profile_test.go:147-148 reads four guides only when
    # DISPATCH_EDIT_PROFILE_OUT is set (line 125), which CI never sets: not a CI read.
    f"{ENVOY}/cmd/dispatch/pod_grace_period_test.go": reads((f"{INFRA}/gcp/testdata/dispatch-pod.golden", "cmd/dispatch/pod_grace_period_test.go:17")),
    f"{ENVOY}/cmd/dispatch/settings_test.go": reads(
        (f"{ENVOY}/cmd/dispatch/**", "cmd/dispatch/settings_test.go's environmentReads walks .go files"),
        (f"{ENVOY}/internal/dispatch/**", "cmd/dispatch/settings_test.go's environmentReads walks .go files")),
    f"{ENVOY}/cmd/listener/api_test.go": reads((f"{FIX}/dispatch-targeted-delivery.json", "cmd/listener/api_test.go:191,436,480")),
    f"{ENVOY}/internal/broker/config/config_test.go": reads((f"{ENVOY}/docker/rds-global-bundle.pem", "internal/broker/config/config_test.go:573")),
    f"{ENVOY}/internal/broker/oauth/claims_doc_test.go": reads(("docs/kubernetes.md", "internal/broker/oauth/claims_doc_test.go:161")),
    f"{ENVOY}/internal/cistore/settlement_fixture_test.go": reads((f"{FIX}/checks-settlement/*", "internal/cistore/settlement_fixture_test.go:56")),
    f"{ENVOY}/internal/contracts/dedupe_test.go": reads((f"{FIX}/dedupe-keys.json", "internal/contracts/dedupe_test.go:14")),
    f"{ENVOY}/internal/contracts/golden_test.go": reads(
        (f"{ENVOY}/scripts/fixtures/github/*", "internal/contracts/golden_test.go:47"),
        (f"{FIX}/github-envelopes/*", "internal/contracts/golden_test.go:72")),
    f"{ENVOY}/internal/contracts/normalize_test.go": reads(
        (f"{FIX}/github-envelopes/*", "internal/contracts/normalize_test.go:689"),
        (f"{ENVOY}/scripts/fixtures/github/*", "internal/contracts/normalize_test.go:2379")),
    f"{ENVOY}/internal/dispatch/api/copy_lock_order_test.go": [
        run(ENVOY, ["./cmd/dispatch"], "internal/dispatch/api/copy_lock_order_test.go:263 go list -deps -export ./cmd/dispatch in packages/envoy", mode="build")],
    f"{ENVOY}/internal/dispatch/api/delivery_live_diff_test.go": reads((f"{FIX}/live/*", "internal/dispatch/api/delivery_live_diff_test.go:982,1011")),
    f"{ENVOY}/internal/dispatch/api/delivery_measures_test.go": reads(
        (f"{ENVOY}/internal/dispatch/delivery/measures/testdata/dora-fixture.json", "internal/dispatch/api/delivery_measures_test.go:82")),
    f"{ENVOY}/internal/dispatch/api/delivery_test.go": reads(
        (f"{ENVOY}/internal/dispatch/delivery/testdata/dev-only-run-jobs.json", "internal/dispatch/api/delivery_test.go:714")),
    f"{ENVOY}/internal/dispatch/api/lock_level_test.go": MODULE_SOURCES,
    f"{ENVOY}/internal/dispatch/api/event_insert_guard_test.go": MODULE_SOURCES,
    f"{ENVOY}/internal/dispatch/api/review_write_guard_test.go": MODULE_SOURCES,
    f"{ENVOY}/internal/dispatch/docs/approval_carry_test.go": reads((f"{FIX}/decision-blocks/*", "internal/dispatch/docs/approval_carry_test.go:26")),
    f"{ENVOY}/internal/dispatch/docs/history_fold_test.go": reads(
        (f"{ENVOY}/internal/dispatch/pmdoc/testdata/fixtures.json", "internal/dispatch/docs/history_fold_test.go:478")),
    f"{ENVOY}/internal/dispatch/pmdoc/update_test.go": [
        Read(g, "internal/dispatch/pmdoc/update_test.go:236-238 bun run gen/<script> (imports @legion/proof-editor)") for g in PMDOC_GEN],
    f"{ENVOY}/internal/dispatch/pmdoc/render_readback_test.go": [
        Read(g, "internal/dispatch/pmdoc/render_readback_test.go:34 bun run gen/differential.ts") for g in PMDOC_GEN],
    f"{ENVOY}/internal/dispatch/store/search_text_test.go": reads(
        (f"{ENVOY}/**", "internal/dispatch/store/search_text_test.go:102-120 walks every .md under packages/envoy (infra included)")),
    f"{ENVOY}/internal/smoke/rdsbundle_test.go": [
        run(ENVOY, ["./internal/smoke/systemroots"], "internal/smoke/rdsbundle_test.go:38 go build ./systemroots CGO_ENABLED=0 GOOS=linux",
            mode="build", env={"CGO_ENABLED": "0", "GOOS": "linux"}),
        Read(f"{ENVOY}/docker/rds-global-bundle.pem", "internal/smoke/rdsbundle_test.go:64")],
    f"{ENVOY}/internal/testnats/nats_test.go": [
        run(ENVOY, ["./..."], "internal/testnats/nats_test.go:708-709 go list -f ... ./... in packages/envoy", mode="build"),
        *[Read(g, "internal/testnats/nats_test.go:741-747 walks every .go file under packages/envoy but infra, node_modules, testdata")
          for g in envoy_top_level_but_infra()]],
    # --- envoy-infra
    f"{INFRA}/core/permissions_live_test.go": [
        run(ENVOY, ["./cmd/dispatch", "./cmd/listener"], "infra/core/permissions_live_test.go's buildEnvoyCommand go build . in ../../cmd/<dispatch|listener> (root go.work)", mode="build")],
    f"{INFRA}/core/allowed_hosts_agreement_test.go": [run(f"{INFRA}/core", ["."], "infra/core/allowed_hosts_agreement_test.go:95 go list -deps .", mode="build")],
    f"{INFRA}/gcp/imports_test.go": [run(f"{INFRA}/gcp", ["."], "infra/gcp/imports_test.go:15 go list -deps .", mode="build")],
    f"{INFRA}/host/heldportwrapper/main_test.go": [
        run(ENVOY, ["./cmd/listener"], "infra/host/heldportwrapper/main_test.go:113-114 go build . in ../../../cmd/listener (root go.work)", mode="build")],
    f"{INFRA}/host/live_test.go": [run(INFRA, ["./cmd/pulumi-resource-envoy-infra"], "infra/host/live_test.go pulumitest.Plugin -> internal/pulumitest/pulumitest.go:168 go build", mode="build")],
}


# ---------------------------------------------------------------- GATES
NOTICES_STEP = "Check every Go release archive's third-party notices can be written and the cross-built commands compile"


def go_notices_runs():
    out = []
    for target in targets(GO_TARGETS, "legion"):
        goos, goarch = target.split("/")
        out.append(step_run("lint", NOTICES_STEP, '../../scripts/go-third-party-notices.sh "$notices/legion-${target%/*}-${target#*/}" ./cmd/legion',
                            DAEMON, ["./cmd/legion"], mode="build", env={"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch}))
    for arch in targets(GO_TARGETS, "envoy"):
        out.append(step_run("lint", NOTICES_STEP, '../../scripts/go-third-party-notices.sh "$notices/legion-envoy-$arch" ./cmd/listener',
                            ENVOY, ["./cmd/listener"], mode="build", env={"GOOS": "linux", "GOARCH": arch}))
        out.append(step_run("lint", NOTICES_STEP, '../../scripts/go-third-party-notices.sh "$notices/agent-secrets-$arch"',
                            ENVOY, ["./cmd/agent-secrets", "./cmd/agent-secrets-helper"], mode="build",
                            env={"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch}))
    for target in targets(CLIENT_TARGETS, "agent-secrets-client"):
        goos, goarch = target.split("/")
        out.append(step_run("lint", NOTICES_STEP, "go build -buildvcs=false -o /dev/null ./cmd/agent-secrets",
                            ENVOY, ["./cmd/agent-secrets"], mode="build", env={"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch}))
    for target in targets(INFRA_TARGETS, "envoy-infra"):
        goos, goarch = target.split("/")
        out.append(step_run("lint", NOTICES_STEP, "go build -buildvcs=false -o /dev/null ./cmd/...",
                            INFRA, ["./cmd/..."], mode="build", env={"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch}))
    return out


# The steps of envoy-go, envoy-smoke and listener-server-change whose go commands are not in the
# simple form, pinned whole.
UNIT_TEST_STEP = "Unit test envoy Go package"
UNIT_TEST_SCRIPT = """\
go test -timeout 20m ./internal/dispatch/api
go list ./... | grep -Ev '/integration|/internal/dispatch/api$' | xargs go test -timeout 20m"""
PACK_STEP = "Pack envoy binaries"
PACK_SCRIPT = """\
for arch in $(jq -r '.envoy[]' ../../.github/go-release-targets.json); do
  out="../../out/pr-artifacts/envoy-$arch"
  mkdir -p "$out"
  GOOS=linux GOARCH=$arch go build -buildvcs=false -o "$out/envoy-listener" ./cmd/listener
  GOOS=linux GOARCH=$arch ../../scripts/go-third-party-notices.sh "$out/THIRD_PARTY_NOTICES" ./cmd/listener
  tar czf "../../out/pr-artifacts/legion-envoy-$arch.tar.gz" -C "$out" envoy-listener THIRD_PARTY_NOTICES
  tar tzvf "../../out/pr-artifacts/legion-envoy-$arch.tar.gz"
done"""
SCHEMA_STEP = "The envoy-infra plugin's schema carries the version it was built with"
SCHEMA_SCRIPT = ('go build -ldflags "-X main.version=1.2.3" -o /tmp/pr ./cmd/pulumi-resource-envoy-infra && '
                 """pulumi package get-schema /tmp/pr | jq -e '.version == "1.2.3"'""")
# The soak's `-run` is not a flag the simple form reduces, and it moves no closure: the whole
# cmd/listener package compiles under the serverchange tag, whichever test the run selects.
SOAK_STEP = "Server-change soak of the listener"
SOAK_SCRIPT = "go test -tags serverchange -count=1 -timeout 15m -v -run TestServerChange ./cmd/listener"


RUN_SERVER = "packages/dispatch/e2e/run-server.sh"
BUILD_BINARIES = "docs/site/scripts/build-binaries.sh"
DISPATCH_SERVER_BUILD = "flock ./.dispatch-e2e.lock go build -o ./dispatch-e2e ./cmd/dispatch"
SIGNIN_ISSUER = "packages/dispatch/e2e/run-signin-issuer.sh"
BROWSER_MP = "dispatch-browser-multiprocess"
BROWSER_MP_SPECS = "Browser-test dispatch dashboard against two server processes"
BROWSER_MP_SIGNIN = "Browser-test a sign-in across two server processes"
STALL = "envoy-session-stall"
STALL_CLIENT = "Stall test of envoy-client's NATS connection options"
LOADTEST_RUN_STEP = "One-run-at-a-time test for loadtest/run-step.sh and run-series.sh (fakes go and uv; no stack)"
LOADTEST_LOCK = "Lock test for loadtest/common.sh (inherited lock, refusals, holders in another PID namespace; no stack)"
CHECK_STEP = "Check every CI filter matches every file its job reads"
COPY_STEP = "Copy every image the package does not hold"
PACKAGE_STEP = "Check the package holds every row of .github/ci-images.yaml"

# docs-pages.yaml builds and publishes main's site in a workflow of its own (its header says why),
# so its on.push.paths stands in for the docs filter on main.
PUSH_WORKFLOWS = {"docs": ".github/workflows/docs-pages.yaml"}

GATES = [
    gate("typecheck", "typecheck", runs=[
        step_run("typecheck", "Vet the Go daemon, its e2e-tagged live proofs included", "go vet -tags e2e ./...", DAEMON, ["./..."],
                 tags="e2e", mode="vet"),
    ], reads=CI_SELF + setup_go("typecheck", f"{DAEMON}/go.mod")),

    gate("test", "test", runs=[
        step_run("test", "Test the Go daemon", "go test -count=1 -timeout 20m ./...", DAEMON, ["./..."]),
        step_run("test", "Test the concurrent packages under the race detector",
                 "go test -count=1 -race ./internal/daemon/ ./internal/record/ ./internal/sessionproxy/ ./internal/launcher/",
                 DAEMON, ["./internal/daemon/", "./internal/record/", "./internal/sessionproxy/", "./internal/launcher/"], race=True),
        script_run("scripts/e2e/stage1-skeleton.sh", 'go build -o "$work/legion" ./cmd/legion', at("test", "Stage 1 live proof"),
                   DAEMON, ["./cmd/legion"], mode="build"),
        script_run("scripts/e2e/lib/held-port.test.ts", 'const build = Bun.spawnSync(["go", "build", "-o", listener, "./cmd/listener"], {',
                   at("test", "bun test scripts/e2e/lib"), ENVOY, ["./cmd/listener"], mode="build"),
    ], reads=CLAUDE_ENVOY_BUILD + reads(
        (".github/scripts/*", f"{at('test', 'Test release scripts')} and the steps after it: each .github/scripts/*.test.sh sources "
                              "test-lib.sh and runs its checker (with workflow_paths.py, check-pr-text.sh, ci_filter_coverage.py, "
                              "ci_job_reads.py or release-bump.sh where a checker reads them)"),
        (".github/e2e-scripts.json", f"{at('test', 'Test the e2e proof selector')}: select-e2e-proofs.test.sh runs the "
                                     "selector over the repository's own list"),
        ("scripts/third-party-notices.test.ts", at("test", "Test third-party notices")),
        ("scripts/third-party-notices.ts", "scripts/third-party-notices.test.ts:5,125; scripts/pi-plugin-prepack.sh:93,96 (packed by promptreferences_omp_test.go)"),
        ("scripts/semantic-line-breaks.test.ts", at("test", "Test the semantic line-break checker")),
        ("scripts/semantic-line-breaks.ts", "scripts/semantic-line-breaks.test.ts:6 spawns it"),
        ("scripts/sessions-import-pods.test.sh", at("test", "Test the session import pods script (a stand-in kubectl)")),
        ("scripts/sessions-import-pods.sh", "scripts/sessions-import-pods.test.sh:11"),
        ("scripts/e2e/**", f"{at('test', 'bun test scripts/e2e/lib')} (lib/script-functions.test.ts:101-104 scans scripts/e2e/*.sh and lib/*); "
                           f"{at('test', 'Stage 1 live proof')}: stage1-skeleton.sh sources lib/rig.sh (with port-file.sh, held-port.py) "
                           f"and runs built-from.sh; {at('test', 'Test the e2e proof selector')}: the selector reads scripts/e2e/*.sh"),
        (CONTRACTS_SRC, "scripts/e2e/lib/envoy-bridge.ts:9-10, session-settings-services.ts:6; scripts/dispatch/ask-census.ts:9 (tsc -p scripts/dispatch)"),
        (ENVOY_CLIENT_SRC, "scripts/e2e/lib/nats-stream.ts:16-18, operator-dispatch.ts:16-21, envoy-bridge.ts:11; scripts/dispatch/ask-census.ts:10-15"),
        ("scripts/dispatch/**", f"{at('test', 'Lint the Dispatch census script')}, typecheck and test"),
        ("scripts/github-apps/**", f"{at('test', 'Lint the GitHub App registration script')} and typecheck"),
        (f"{FIX}/daemon-api/**", "scripts/e2e/lib/stage4b-boot-capabilities.test.ts:16"),
        (f"{DAEMON}/internal/daemon/controller.go", "scripts/e2e/lib/controller-liveness-verdict.test.ts:344-345 reads it as text"),
        (f"{DAEMON}/internal/controller/liveness.go", "scripts/e2e/lib/controller-liveness-verdict.test.ts:348"),
        (f"{DAEMON}/internal/supervise/machine.go", "scripts/e2e/lib/controller-liveness-verdict.test.ts:351"),
        (f"{DAEMON}/internal/intake/consume.go", "scripts/e2e/lib/stage4b-consumers.test.ts:24-27"),
        (f"{DAEMON}/internal/capabilities/testdata/web-search-open-planner.json", "scripts/e2e/lib/stage4b-capabilities.test.ts:13-22"),
        (f"{DAEMON}/internal/runtime/sandbox/testdata/golden/root.json", "scripts/e2e/lib/stage4b-pods.test.ts:22-35"),
        (f"{DAEMON}/internal/runtime/sandbox/testdata/golden/root-operator-route.json", "scripts/e2e/lib/stage4b-pods.test.ts:175-187"),
        ("packages/pi-shared/src/**", "packages/pi-legion/extensions/legion.ts:17-26 imports @legion/pi-shared/*; both packs bundle it"),
        ("packages/envoy-client/bin/dispatch.ts", "pi-envoy pack: scripts/pi-plugin-prepack.sh:92 -> packages/envoy-client/scripts/build-dispatch-cli.sh:17"),
        (".github/actions/install-uv/**", at("test", "./.github/actions/install-uv")),
        (f"{DAEMON}/docker/worker.Dockerfile", ".github/actions/install-uv/action.yml:16-17 ARG UV_VERSION/UV_SHA256"),
        (".github/actions/install-changie/**", at("test", "./.github/actions/install-changie")),
        ("packages/pi-legion/.changie.yaml", ".github/scripts/changelog-fold.test.sh:56 copies it"),
        ("packages/pi-legion/CHANGELOG.md", ".github/scripts/changelog-fold.test.sh:56 copies it"),
        (".github/actions/pull-test-images/**", at("test", "Pull the NATS test container images")),
        (f"{DAEMON}/internal/testnats/nats.go", f"{at('test', 'Pull the NATS test container images')} const Image"),
        (f"{ENVOY}/internal/testnats/nats.go", f"{at('test', 'Pull the NATS test container images')} const HubImage"),
    ) + install_jj("test") + install_omp("test") + setup_go("test", f"{DAEMON}/go.mod")
      + biome(at("test", "Lint the Dispatch census script")) + bun_job("test") + CI_SELF),


    # The job ci.yaml gates is docs, and the steps it runs are docs-site.yaml's (at_docs).
    gate("docs", "docs", runs=[
        script_run(BUILD_BINARIES, 'go -C "$root/$legion_module" build -o "$bin/legion" ./cmd/legion',
                   f"docs/site/scripts/generate.ts:116, run by {at_docs('Build the docs site')}", DAEMON, ["./cmd/legion"], mode="build"),
        script_run(BUILD_BINARIES, 'go -C "$root/$envoy_module" build -o "$bin/envoy-dispatch" ./cmd/dispatch',
                   f"docs/site/scripts/generate.ts:116, run by {at_docs('Build the docs site')}", ENVOY, ["./cmd/dispatch"], mode="build"),
        script_run(BUILD_BINARIES, 'go -C "$root/$envoy_module" build -o "$bin/envoy-broker" ./cmd/broker',
                   f"docs/site/scripts/generate.ts:116, run by {at_docs('Build the docs site')}", ENVOY, ["./cmd/broker"], mode="build"),
        script_run(BUILD_BINARIES, 'go -C "$root/$envoy_module" build -o "$bin/" ./cmd/agent-secrets ./cmd/agent-secrets-helper ./cmd/broker-refgen',
                   f"docs/site/scripts/generate.ts:116, run by {at_docs('Build the docs site')}", ENVOY,
                   ["./cmd/agent-secrets", "./cmd/agent-secrets-helper", "./cmd/broker-refgen"], mode="build"),
        script_run(RUN_SERVER, DISPATCH_SERVER_BUILD, f"started by packages/dispatch/e2e/harness-listeners.ts's dispatchServer (harness-start.ts, which "
                   f"docs/site/media/harness.ts:21,98 imports), run by {at_docs('Take the screenshots')}",
                   ENVOY, ["./cmd/dispatch"], mode="build"),
    ], reads=reads(
        ("docs/site/**", f"{at_docs('Lint the docs site')}, typecheck, screenshots (docs/site/media/shots.ts) and build (generate.ts + astro build) in docs/site"),
        ("packages/dispatch/**", f"{at_docs('Build the Dispatch dashboard the screenshots show')}; docs/site/media/harness.ts:21,98 boots packages/dispatch/e2e/harness-start.ts"),
        ("packages/proof-editor/src/**", "packages/dispatch/web/vite.config.ts:104-123 aliases (build:web)"),
        (CONTRACTS_SRC, "packages/dispatch/web/vite.config.ts:94-103; docs/site/generators/dispatch-tools.ts:7"),
        (ENVOY_CLIENT_SRC, "docs/site/generators/dispatch-tools.ts:12 imports ../../../packages/envoy-client/src/dispatch-command.ts"),
        ("skills/**", "docs/site/generators/legion-skills.ts:11-17 reads skills/*/SKILL.md"),
        ("deploy/kubernetes/**", "docs/site/generators/legion-config.sh:28-32 renders every tracked deploy/kubernetes *.example/*.yml/*.yaml"),
        ("scripts/third-party-notices.ts", "docs/site/astro.config.mjs:15; packages/dispatch/web/vite.config.ts:11"),
        (".github/scripts/check-private-names.sh", at_docs("Check the built site has no private name, internal host, or tailnet host")),
        (f"{ENVOY}/internal/dispatch/api/routes_table.go", "docs/site/generators/dispatch-api.ts:80 sources, checked by generators/lib/markdown.ts:46-49"),
        (f"{ENVOY}/cmd/dispatch/settings.go", "docs/site/generators/dispatch-config.ts:65 sources, checked by generators/lib/markdown.ts:46-49"),
        (BUILD_BINARIES, f"docs/site/scripts/generate.ts:116 runs it ({at_docs('Build the docs site')})"),
        (RUN_SERVER, f"packages/dispatch/e2e/harness-listeners.ts's dispatchServer runs it ({at_docs('Take the screenshots')})"),
        ("scripts/e2e/lib/held-port.py", "packages/dispatch/e2e/run-server.sh:80 holds the server's port through it"),
        # broker-refgen (docs/site/generators/broker-reference.sh:14) parses these at run time.
        (f"{ENVOY}/internal/broker/api/*", "broker-refgen parses it (packages/envoy/cmd/broker-refgen/routes.go:15,173)"),
        (f"{ENVOY}/internal/broker/config/*", "broker-refgen parses it (packages/envoy/cmd/broker-refgen/config.go:11,36)"),
        (f"{ENVOY}/cmd/broker/*", "broker-refgen parses it (packages/envoy/cmd/broker-refgen/config.go:12,40)"),
        (f"{ENVOY}/internal/broker/helper/*", "broker-refgen parses it (packages/envoy/cmd/broker-refgen/pages.go:16, codes.go:15)"),
        (f"{ENVOY}/cmd/agent-secrets/*", "broker-refgen parses it (packages/envoy/cmd/broker-refgen/pages.go:17, codes.go:68)"),
    ) + setup_go(DOCS_JOB, f"{DAEMON}/go.mod", DOCS_SITE) + biome(at_docs("Lint the docs site"))
      + bun_job(DOCS_JOB, DOCS_SITE) + CI_SELF + DOCS_SITE_SELF),

    gate("contracts", "contracts", reads=reads(
        ("packages/contracts/**", f"{at('contracts', 'Lint contracts')}, typecheck, test (src/, scripts/gen-go.test.ts) and build in packages/contracts"),
        ("skills/dispatch/SKILL.md", "packages/contracts/src/dispatch-tools.test.ts's dispatchSkillHeadings reads skills/dispatch/SKILL.md headings"),
        ("skills/dispatch-writing-specs/SKILL.md", "packages/contracts/src/dispatch-tools.test.ts:1056 existsSync(skills/dispatch-writing-specs/SKILL.md)"),
        (f"{ENVOY}/internal/dispatch/text/testdata/dispatch-href-references.json", "packages/contracts/src/dispatch-href.test.ts:49-53"),
        (f"{ENVOY}/internal/dispatch/text/testdata/dispatch-text-references.json", "packages/contracts/src/dispatch-text-references.test.ts:10-14"),
        (f"{ENVOY}/internal/contracts/generated.go", "packages/contracts/scripts/gen-go.test.ts:6,48-50 and gen-go.ts:516 (--check compares the committed file)"),
        (f"{DAEMON}/internal/dispatch/generated.go", "packages/contracts/scripts/gen-go.test.ts:7,48-50 and gen-go.ts:520"),
    ) + biome(at("contracts", "Lint contracts")) + bun_job("contracts") + CI_SELF),

    gate("envoy_client", "envoy-client", reads=reads(
        ("packages/envoy-client/**", f"{at('envoy-client', 'Lint Envoy client')}, typecheck, test and build in packages/envoy-client"),
        (CONTRACTS_SRC, f"{at('envoy-client', 'Build contracts for Envoy client')}; tsconfig paths @legion/contracts -> ../contracts/src/index.ts"),
        (f"{FIX}/dispatch-targeted-delivery.json", "packages/envoy-client/src/__tests__/delivery.test.ts:16-17, dispatch-text.test.ts:117-118"),
        (f"{FIX}/dispatch-public-url.json", "packages/envoy-client/src/__tests__/dispatch-config.test.ts:5"),
        (f"{ENVOY}/internal/dispatch/api/testdata/**", "packages/envoy-client/src/__tests__/askgate-new-asks.test.ts:14-21 reads the Dispatch server's table of decision-block inserts, which packages/envoy/internal/dispatch/api's TestDocumentEditAdviceCountsTheAskBlocksTheEditAdded reads too"),
        ("skills/**", "packages/envoy-client/src/__tests__/dispatch-advice.test.ts:683-690 reads skills/<skill>/SKILL.md for every skill a notice cites"),
    ) + biome(at("envoy-client", "Lint Envoy client")) + bun_job("envoy-client") + CI_SELF),

    gate("pi_shared", "pi-shared", reads=reads(
        ("packages/pi-shared/**", f"{at('pi-shared', 'Lint pi-shared')}, typecheck and test in packages/pi-shared"),
        (ENVOY_CLIENT_SRC, "packages/pi-shared/tsconfig.json paths @legion/envoy-client/errors; src imports @legion/envoy-client/* (exports.bun -> src)"),
        (CONTRACTS_SRC, "pi-shared and envoy-client import @legion/contracts (contracts exports.bun -> src)"),
        (f"{FIX}/daemon-api/**", "packages/pi-shared/src/daemon-client.test.ts:15, legion-claim-core.test.ts:12, legion-phase-ops.test.ts:21"),
        ("skills/**", "packages/pi-shared/test/skills-partition.test.ts:11-18,40-41 reads every skills/ directory and stages both partitions"),
        ("scripts/pi-plugin-prepack.sh", "packages/pi-shared/test/skills-guard.ts:17,34 runs scripts/pi-plugin-prepack.sh --stage-skills"),
        (f"{DAEMON}/internal/prompts/**", "packages/pi-shared/test/skills-partition.test.ts:57-63 reads every .md under packages/daemon/internal/prompts"),
        (".github/workflows/release.yaml", "packages/pi-shared/test/skills-partition.test.ts:82"),
    ) + biome(at("pi-shared", "Lint pi-shared")) + bun_job("pi-shared") + CI_SELF),

    gate("pi_envoy", "pi-envoy", reads=reads(
        ("packages/pi-envoy/**", f"{at('pi-envoy', 'Lint pi-envoy')}, typecheck, test and pack in packages/pi-envoy"),
        ("packages/pi-shared/src/**", "packages/pi-envoy/tsconfig.json paths @legion/pi-shared/* and include ../pi-shared/src/omp-host.d.ts; the bundle inlines it"),
        ("packages/pi-shared/test/**", "packages/pi-envoy/extensions/dispatch-first-omp.test.ts:13, src/skills-guard.test.ts:23, scripts/prepack.test.ts:6"),
        (ENVOY_CLIENT_SRC, "packages/pi-envoy/tsconfig.json paths @legion/envoy-client/*; extensions/envoy.ts:19-101 imports"),
        ("packages/envoy-client/bin/dispatch.ts", "packages/pi-envoy/package.json:23 build and scripts/pi-plugin-prepack.sh:92 run build-dispatch-cli.sh, entry bin/dispatch.ts"),
        ("packages/envoy-client/scripts/build-dispatch-cli.sh", "packages/pi-envoy/package.json:23; scripts/pi-plugin-prepack.sh:92"),
        (CONTRACTS_SRC, f"{at('pi-envoy', 'Build contracts for pi-envoy')}; tsconfig paths @legion/contracts"),
        (f"{FIX}/dispatch-targeted-delivery.json", "packages/pi-envoy/extensions/envoy.test.ts:208"),
        ("skills/**", "packages/pi-envoy/src/skills-guard.test.ts:29-32; scripts/pi-plugin-prepack.sh:99 stages skills; extensions/envoy.ts's resolveSkillsDirectory"),
        ("scripts/pi-plugin-prepack.sh", f"packages/pi-envoy/package.json:24 prepack ({at('pi-envoy', 'Pack pi-envoy')}); scripts/prepack.test.ts:73"),
        ("scripts/third-party-notices.ts", "scripts/pi-plugin-prepack.sh:93 writes dist/THIRD_PARTY_NOTICES"),
        ("scripts/e2e/lib/pack-plugin.sh", "packages/pi-envoy/scripts/prepack.test.ts:92-102"),
        (f"{DAEMON}/internal/prompts/**", "packages/pi-envoy/src/skills-guard.test.ts:30-33 (skillAndPromptRoots)"),
    ) + install_jj("pi-envoy") + install_omp("pi-envoy") + biome(at("pi-envoy", "Lint pi-envoy")) + bun_job("pi-envoy") + CI_SELF),

    gate("pi_legion", "pi-legion", runs=[
        script_run("packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts",
                   'const result = Bun.spawnSync(["go", "run", "-overlay", overlay, "./cmd/daemon-pane", ...args], {',
                   f"daemon-pane.test.ts, run by {at('pi-legion', 'Test pi-legion')}", DAEMON, ["./cmd/daemon-pane"], mode="build",
                   overlay={f"{DAEMON}/cmd/daemon-pane/main.go": "packages/pi-legion/scripts/skill-scenarios/daemon-pane.go"}),
    ], reads=reads(
        ("packages/pi-legion/**", f"{at('pi-legion', 'Lint pi-legion')}, typecheck, test and pack in packages/pi-legion"),
        ("packages/pi-envoy/**", "packages/pi-legion/extensions/legion.test.ts:143 and legion-role-claim.test.ts:61 import ../../pi-envoy/extensions/envoy; packages/pi-shared/test/omp-harness.ts:662"),
        ("packages/pi-shared/src/**", "packages/pi-legion/tsconfig.json paths @legion/pi-shared/* and include ../pi-shared/src/omp-host.d.ts"),
        ("packages/pi-shared/test/**", "packages/pi-legion/extensions/legion-phase-stall-omp.test.ts:12, legion-role-tools-omp.test.ts:12, src/skills-guard.test.ts:14, scripts/prepack.test.ts:4"),
        (ENVOY_CLIENT_SRC, "packages/pi-legion/tsconfig.json paths @legion/envoy-client/*; scripts/skill-scenarios/*.ts import @legion/envoy-client/*"),
        (CONTRACTS_SRC, f"{at('pi-legion', 'Build contracts for pi-legion')}; tsconfig paths @legion/contracts"),
        (f"{FIX}/daemon-api/**", "packages/pi-legion/extensions/legion.test.ts:1061,2738, legion-role-claim.test.ts:12, src/daemon-api-version.test.ts:14"),
        (f"{FIX}/capability-redaction.json", "packages/pi-legion/src/capability-report.test.ts:768-770"),
        (f"{FIX}/capability-redaction-plugin-report.json", "packages/pi-legion/src/capability-report.test.ts:776,874"),
        ("packages/dispatch/e2e/api.ts", "packages/pi-legion/scripts/skill-scenarios/seed.ts:96 (typecheck includes scripts/skill-scenarios/*.ts)"),
        ("packages/dispatch/e2e/harness-ports.ts", "packages/dispatch/e2e/api.ts:52"),
        ("packages/dispatch/web/src/api/types.ts", "packages/dispatch/e2e/api.ts:51"),
        ("skills/**", "packages/pi-legion/src/skills-guard.test.ts:23,57,142; scripts/skill-scenarios/rig.test.ts:791,1004; prepack stages skills"),
        ("scripts/pi-plugin-prepack.sh", f"packages/pi-legion/package.json prepack ({at('pi-legion', 'Pack pi-legion')}); packages/pi-shared/test/skills-guard.ts:17"),
        ("scripts/third-party-notices.ts", "scripts/pi-plugin-prepack.sh:96"),
        (f"{DAEMON}/internal/**", "packages/pi-legion/src/skills-guard.test.ts:24 (prompts root) and :144 sweeps every .md/.go/.ts/.yaml/.yml/.example/.sh under packages/daemon/internal"),
        ("docs/kubernetes.md", "packages/pi-legion/src/skills-guard.test.ts:143"),
        ("docs/site/src/content/docs/**", "packages/pi-legion/src/skills-guard.test.ts:145"),
        ("deploy/kubernetes/daemon/controller.yaml.example", "packages/pi-legion/src/skills-guard.test.ts:146"),
        ("AGENTS.md", "packages/pi-legion/src/skills-guard.test.ts:147"),
        ("README.md", "packages/pi-legion/src/skills-guard.test.ts:148"),
        ("scripts/e2e/lib/**", "packages/pi-legion/scripts/skill-scenarios/rig.test.ts:13,129,1009; rig.sh:239 sources scripts/e2e/lib/rig.sh"),
        (".omp-pin", "packages/pi-legion/scripts/skill-scenarios/rig.test.ts:1004-1006 copies .omp-pin"),
        ("packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts", "writes the overlay and runs go run (:27)"),
    ) + install_jj("pi-legion") + install_omp("pi-legion") + setup_go("pi-legion", f"{DAEMON}/go.mod")
      + biome(at("pi-legion", "Lint pi-legion")) + bun_job("pi-legion") + CI_SELF),

    gate("dispatch", "dispatch", runs=[
        script_run(RUN_SERVER, DISPATCH_SERVER_BUILD, f"started by packages/dispatch/e2e/harness-listeners.ts's dispatchServer, which e2e/run.ts starts in "
                   f"{at('dispatch', 'Browser-test dispatch dashboard')}", ENVOY, ["./cmd/dispatch"], mode="build"),
    ], reads=reads(
        ("packages/dispatch/**", f"{at('dispatch', 'Lint dispatch dashboard')}, typecheck, test, build, check:classes, check:asset-sizes and Playwright in packages/dispatch"),
        ("packages/proof-editor/**", f"{at('dispatch', 'Lint proof editor')}, typecheck and test; packages/dispatch/web/vite.config.ts:104-123"),
        (CONTRACTS_SRC, f"{at('dispatch', 'Build contracts for dispatch e2e')}; packages/dispatch/web/vite.config.ts:94-103 aliases"),
        (f"{FIX}/live/**", "packages/dispatch/web/src/features/delivery/lib/applyDelta.test.ts:2-4, e2e/__tests__/delivery-live-probe.test.ts:8-12"),
        (f"{FIX}/decision-blocks/**", "packages/dispatch/web/src/features/doc/decision-blocks.test.ts:10, features/inbox/AskCard.test.tsx:411"),
        ("packages/envoy-client/bin/dispatch.ts", "packages/dispatch/e2e/cli-body-cap.e2e.ts:17 runs it through e2e/dispatch-cli.ts:8"),
        (ENVOY_CLIENT_SRC, "imported by packages/envoy-client/bin/dispatch.ts, which cli-body-cap.e2e.ts:17 runs"),
        (f"{ENVOY}/internal/dispatch/pmdoc/schema/blocks.json", "packages/dispatch/web/src/__tests__/setup.ts:7, features/refs/markdown-engine.test.ts:3, e2e/engine-reading.ts:10"),
        (f"{ENVOY}/scripts/fixtures/shell-quote.json", "packages/dispatch/e2e/__tests__/local-instance-support.test.ts:154-155"),
        (f"{ENVOY}/internal/tests3/seaweedfs.go", f"{at('dispatch', 'Pull the file store image')} names it to the action, and "
         f"{at('dispatch', 'Start an S3-compatible server for uploaded files')} reads const Image; "
         f"{at('dispatch', 'Wait for the S3-compatible server')} waits for the container it started"),
        (".github/actions/pull-test-images/**", at("dispatch", "Pull the file store image")),
        ("scripts/e2e/lib/held-port.py", "packages/dispatch/e2e/run-server.sh:80 holds each lane's server port through it"),
        ("bun.lock", f"{at('dispatch', 'playwright-cache')} keys the browser cache on hashFiles('bun.lock'), "
         f"which pins the Playwright version whose browsers {at('dispatch', 'Install Playwright Chromium, WebKit and Firefox')} downloads"),
        ("scripts/third-party-notices.ts", "packages/dispatch/web/vite.config.ts:11 (build:web writes dist/THIRD_PARTY_NOTICES.txt)"),
    ) + setup_go("dispatch", f"{ENVOY}/go.mod") + biome(at("dispatch", "Lint dispatch dashboard")) + bun_job("dispatch") + CI_SELF),

    # Its "Build the Dispatch server" step names an output (-o), which the simple form does not
    # reduce, so the step is pinned whole.
    gate("dispatch_browser_multiprocess", BROWSER_MP, runs=[
        script_run(RUN_SERVER, DISPATCH_SERVER_BUILD, f"started by packages/dispatch/e2e/harness-listeners.ts's dispatchServer, which e2e/run.ts starts in "
                   f"{at(BROWSER_MP, BROWSER_MP_SPECS)} and {at(BROWSER_MP, BROWSER_MP_SIGNIN)}", ENVOY, ["./cmd/dispatch"], mode="build"),
        script_run(SIGNIN_ISSUER, "flock ./.dev-oidc-issuer-e2e.lock go build -o ./dev-oidc-issuer-e2e ./cmd/dev-oidc-issuer",
                   f"started by packages/dispatch/e2e/harness-listeners.ts's signInIssuerListeners under DISPATCH_E2E_SIGNIN=oidc, in {at(BROWSER_MP, BROWSER_MP_SIGNIN)}",
                   ENVOY, ["./cmd/dev-oidc-issuer"], mode="build"),
    ], exact=[
        pinned_step(BROWSER_MP, "Build the Dispatch server", ENVOY, "go build -o ./dispatch-e2e ./cmd/dispatch", [
            (ENVOY, ["./cmd/dispatch"], "go build -o ./dispatch-e2e ./cmd/dispatch", {"mode": "build"}),
        ]),
    ], reads=reads(
        ("packages/dispatch/**", f"{at(BROWSER_MP, BROWSER_MP_SPECS)} (build:web twice and Playwright) and {at(BROWSER_MP, BROWSER_MP_SIGNIN)} in packages/dispatch"),
        ("packages/proof-editor/**", "packages/dispatch/web/vite.config.ts:104-123 (build:web)"),
        (CONTRACTS_SRC, f"{at(BROWSER_MP, 'Build contracts for dispatch e2e')}; packages/dispatch/web/vite.config.ts:94-103 aliases"),
        (f"{ENVOY}/internal/dispatch/pmdoc/schema/blocks.json", "packages/dispatch/e2e/engine-reading.ts:10"),
        ("scripts/e2e/lib/held-port.py", "packages/dispatch/e2e/run-server.sh:80 holds the server's port through it"),
        ("scripts/third-party-notices.ts", "packages/dispatch/web/vite.config.ts:11 (build:web writes dist/THIRD_PARTY_NOTICES.txt)"),
    ) + setup_go(BROWSER_MP, f"{ENVOY}/go.mod") + bun_job(BROWSER_MP) + CI_SELF),

    gate("claude_envoy", "claude-envoy", reads=CLAUDE_ENVOY_BUILD + biome(at("claude-envoy", "Lint Claude Envoy plugin")) + bun_job("claude-envoy") + CI_SELF),

    gate("envoy_go", "envoy-go", runs=[
        step_run("envoy-go", "Integration test envoy (a real NATS server, in the test process)",
                 "go test ./internal/integration/ -timeout 300s -count=1", ENVOY, ["./internal/integration/"]),
        step_run("envoy-go", "Test the container-backed NATS matrix (natslive)", "go test -tags natslive ./internal/testnats/", ENVOY,
                 ["./internal/testnats/"], tags="natslive"),
        step_run("envoy-go", "Build envoy services", "go build ./cmd/dispatch ./cmd/listener", ENVOY, ["./cmd/dispatch", "./cmd/listener"],
                 mode="build"),
    ], exact=[
        # The API package first, on its own (582cb2a2: its Postgres-backed tests' cleanup queues
        # on the service's one checkpointer beside the other package binaries), then ./... but it
        # and internal/integration, which the integration step tests: counted as all of ./....
        pinned_step("envoy-go", UNIT_TEST_STEP, ENVOY, UNIT_TEST_SCRIPT, [
            (ENVOY, ["./internal/dispatch/api"], "go test -timeout 20m ./internal/dispatch/api", {}),
            (ENVOY, ["./..."], "go list ./... piped into go test, counted as go test ./...", {}),
        ]),
        # One linux build of the listener per architecture the loop reads from the target list.
        pinned_step("envoy-go", PACK_STEP, ENVOY, PACK_SCRIPT, [
            (ENVOY, ["./cmd/listener"], f"GOOS=linux GOARCH={arch} go build ./cmd/listener",
             {"mode": "build", "env": {"GOOS": "linux", "GOARCH": arch}})
            for arch in targets(GO_TARGETS, "envoy")
        ]),
    ], reads=reads(
        (f"{ENVOY}/**", f"{at('envoy-go', 'Check Go formatting')}: gofmt -l . in packages/envoy reads every .go file under it, infra included; the script tests below live here too"),
        ("packages/contracts/scripts/gen-go.ts", f"{at('envoy-go', 'Check Go contracts are generated from the shared schema')} (packages/contracts/package.json check:go)"),
        ("packages/contracts/schemas/**", "packages/contracts/scripts/gen-go.ts:65 reads schemas/envelope.schema.json"),
        (CONTRACTS_SRC, "packages/contracts/scripts/gen-go.ts:4-27 imports ../src/*"),
        (f"{DAEMON}/internal/dispatch/generated.go", "packages/contracts/scripts/gen-go.ts:520,528 --check compares it"),
        (f"{ENVOY}/internal/dispatch/pmdoc/gen/**", f"{at('envoy-go', 'Check pmdoc fixtures are up to date')} (gen.ts --check)"),
        ("packages/proof-editor/src/**", "packages/envoy/internal/dispatch/pmdoc/gen/gen.ts:4-5 imports @legion/proof-editor (exports -> src/lib.ts, src/lib-headless.ts)"),
        (".github/actions/pull-test-images/**", at("envoy-go", "Pull test container images")),
        (f"{ENVOY}/loadtest/run-step.test.sh", at("envoy-go", LOADTEST_RUN_STEP)),
        (f"{ENVOY}/loadtest/run-step.sh", "packages/envoy/loadtest/run-step.test.sh runs it"),
        (f"{ENVOY}/loadtest/run-series.sh", "packages/envoy/loadtest/run-step.test.sh runs it"),
        (f"{ENVOY}/loadtest/common.test.sh", at("envoy-go", LOADTEST_LOCK)),
        (f"{ENVOY}/loadtest/common.sh", "packages/envoy/loadtest/common.test.sh sources it; run-step.sh and run-series.sh source it"),
        (NOTICES, f"{at('envoy-go', 'Pack envoy binaries')} runs it"),
        (GO_TARGETS, f"{at('envoy-go', 'Pack envoy binaries')}: jq -r '.envoy[]'"),
    ) + setup_go("envoy-go", f"{ENVOY}/go.mod") + bun_job("envoy-go") + CI_SELF),


    gate("envoy_infra_go", "envoy-infra-go", runs=[
        step_run("envoy-infra-go", "Vet envoy-infra, its live-test tags included", "go vet -tags pulumilive,natslive ./...", INFRA, ["./..."],
                 tags="pulumilive,natslive", mode="vet"),
        step_run("envoy-infra-go", "Unit test envoy-infra", "go test ./...", INFRA, ["./..."]),
        step_run("envoy-infra-go", "Live tests of envoy-infra against nats-server (natslive)", "go test -timeout 30m -tags natslive ./core/ ./seed/ ./gcp/", INFRA,
                 ["./core/", "./seed/", "./gcp/"], tags="natslive"),
    ], reads=reads(
        (f"{INFRA}/**", f"{at('envoy-infra-go', 'Check Go formatting')}: gofmt -l . in packages/envoy/infra reads every .go file under it"),
        (".github/actions/pull-test-images/**", at("envoy-infra-go", "Pull the NATS and Postgres test container images")),
        (f"{ENVOY}/internal/testnats/nats.go", f"{at('envoy-infra-go', 'Pull the NATS and Postgres test container images')}: consts Image, HubImage"),
        (f"{ENVOY}/internal/testpg/testpg.go", f"{at('envoy-infra-go', 'Pull the NATS and Postgres test container images')}: const Image"),
    ) + setup_go("envoy-infra-go", f"{INFRA}/go.mod") + CI_SELF),

    gate("envoy_go_race", "envoy-go-race", runs=[
        step_run("envoy-go-race", "Race-test delivery, the store, the bus, the listener, the registry store, the session cache and kvwatch",
                 "go test -race -timeout 30m ./internal/dispatch/delivery/... ./internal/dispatch/store/... ./internal/bus/ ./cmd/listener/ ./internal/store/ ./internal/session/ ./internal/kvwatch/...",
                 ENVOY, ["./internal/dispatch/delivery/...", "./internal/dispatch/store/...", "./internal/bus/",
                         "./cmd/listener/", "./internal/store/", "./internal/session/", "./internal/kvwatch/..."], race=True),
    ], reads=setup_go("envoy-go-race", f"{ENVOY}/go.mod") + CI_SELF),

    gate("envoy_go_race_pmdoc", "envoy-go-race-pmdoc", runs=[
        step_run("envoy-go-race-pmdoc", "Race-test pmdoc",
                 "go test -race -timeout 40m ./internal/dispatch/pmdoc/...",
                 ENVOY, ["./internal/dispatch/pmdoc/..."], race=True),
    ], reads=reads(
        (f"{ENVOY}/internal/dispatch/pmdoc/schema/blocks.json", "packages/envoy/internal/dispatch/pmdoc/gen/decode.ts:16, differential.ts:21, edit-blocks.ts:21"),
        ("packages/proof-editor/src/**", "packages/envoy/internal/dispatch/pmdoc/gen/decode.ts:6, differential.ts:12-13, edit-blocks.ts:10 import @legion/proof-editor"),
    ) + setup_go("envoy-go-race-pmdoc", f"{ENVOY}/go.mod") + bun_job("envoy-go-race-pmdoc") + CI_SELF),

    gate("envoy_go_race_api", "envoy-go-race-api", runs=[
        step_run("envoy-go-race-api", "Race-test the Dispatch API",
                 "go test -race -timeout 30m ./internal/dispatch/api/",
                 ENVOY, ["./internal/dispatch/api/"], race=True),
    ], reads=setup_go("envoy-go-race-api", f"{ENVOY}/go.mod") + CI_SELF),

    gate("envoy_go_race_docs", "envoy-go-race-docs", runs=[
        step_run("envoy-go-race-docs", "Race-test the document service",
                 "go test -race -timeout 60m ./internal/dispatch/docs/", ENVOY,
                 ["./internal/dispatch/docs/"], race=True),
    ], reads=setup_go("envoy-go-race-docs", f"{ENVOY}/go.mod") + CI_SELF),

    gate("dispatch_memory", "dispatch-memory", runs=[
        step_run("dispatch-memory", "Memory tests of the Dispatch server", "go test -tags memory -count=1 -timeout 60m -v ./cmd/dispatch",
                 ENVOY, ["./cmd/dispatch"], tags="memory"),
    ], reads=setup_go("dispatch-memory", f"{ENVOY}/go.mod") + CI_SELF),

    gate("dispatch_multiprocess", "dispatch-multiprocess", runs=[
        step_run("dispatch-multiprocess", "Two-process tests of the Dispatch server",
                 "go test -tags multiprocess -count=1 -timeout 20m -v ./cmd/dispatch", ENVOY, ["./cmd/dispatch"], tags="multiprocess"),
    ], reads=setup_go("dispatch-multiprocess", f"{ENVOY}/go.mod") + CI_SELF),

    gate("listener_server_change", "listener-server-change", exact=[
        pinned_step("listener-server-change", SOAK_STEP, ENVOY, SOAK_SCRIPT, [
            (ENVOY, ["./cmd/listener"], "go test -tags serverchange ./cmd/listener", {"tags": "serverchange"}),
        ]),
    ], reads=setup_go("listener-server-change", f"{ENVOY}/go.mod") + CI_SELF),

    gate("broker_multiprocess", "broker-multiprocess", runs=[
        step_run("broker-multiprocess", "Two-process tests of the secrets broker",
                 "go test -tags multiprocess -count=1 -timeout 20m -v ./cmd/broker", ENVOY, ["./cmd/broker"], tags="multiprocess"),
    ], reads=setup_go("broker-multiprocess", f"{ENVOY}/go.mod") + CI_SELF),


    # The listener image this job builds and smokes is left to check-image-trigger-paths.sh,
    # which proves envoy_smoke covers every file packages/envoy/docker/Dockerfile builds from
    # (the Dockerfile the base-images step reads among them). This gate holds the filter to the
    # rest: the job's go commands, run on the host, and the step reads that are not image inputs.
    gate("envoy_smoke", "envoy-smoke", runs=[
        step_run("envoy-smoke", "Container smoke test (testcontainers)", "go test -tags smoke -v -timeout 5m ./internal/smoke/",
                 ENVOY, ["./internal/smoke/"], tags="smoke"),
        step_run("envoy-smoke", "Live tests of envoy-infra's host:Listener", "go test -tags pulumilive -v -timeout 15m ./host/",
                 INFRA, ["./host/"], tags="pulumilive"),
    ], exact=[
        pinned_step("envoy-smoke", SCHEMA_STEP, INFRA, SCHEMA_SCRIPT, [
            (INFRA, ["./cmd/pulumi-resource-envoy-infra"], "go build ./cmd/pulumi-resource-envoy-infra", {"mode": "build"}),
        ]),
    ], reads=reads(
        (".github/actions/pull-test-images/**", at("envoy-smoke", "Pull test container images")),
        (f"{ENVOY}/internal/testnats/nats.go", f"{at('envoy-smoke', 'Pull test container images')}: const Image"),
        (".github/actions/install-pulumi/**", at("envoy-smoke", "./.github/actions/install-pulumi")),
        (f"{INFRA}/internal/pulumitest/pulumitest.go",
         f"{at('envoy-smoke', 'Download the docker provider plugin the live tests pin')}: sed reads const DockerPluginVersion"),
    ) + setup_go("envoy-smoke", f"{ENVOY}/go.mod") + CI_SELF),

    gate("envoy_e2e_local", "envoy-e2e-local", runs=[
        script_run(f"{ENVOY}/scripts/e2e-local.sh", 'go build -o "$listener_binary" ./cmd/listener',
                   f"run by {at('envoy-e2e-local', 'Local end-to-end acceptance')}", ENVOY, ["./cmd/listener"], mode="build"),
        script_run(f"{ENVOY}/scripts/e2e-local.sh", 'go build -o "$nats_binary" ./internal/testnats/cmd/natsd',
                   f"run by {at('envoy-e2e-local', 'Local end-to-end acceptance')}", ENVOY, ["./internal/testnats/cmd/natsd"], mode="build"),
    ], reads=reads(
        (f"{ENVOY}/scripts/e2e-local.test.sh", at("envoy-e2e-local", "Refusal test for e2e-local.sh")),
        (f"{ENVOY}/scripts/e2e-local.sh", f"{at('envoy-e2e-local', 'Local end-to-end acceptance')}; e2e-local.test.sh:6"),
        (f"{ENVOY}/scripts/fixtures/github/**", "packages/envoy/scripts/e2e-local.sh:52,547-578 posts each fixture"),
        (ENVOY_CLIENT_SRC, "packages/envoy/scripts/e2e-local.sh:582-587 runs bun in packages/envoy-client importing @legion/envoy-client/delivery"),
        (CONTRACTS_SRC, "packages/envoy-client/src/delivery.ts imports @legion/contracts"),
    ) + setup_go("envoy-e2e-local", f"{ENVOY}/go.mod") + bun_job("envoy-e2e-local") + CI_SELF),

    gate("envoy_session_stall", STALL, runs=[
        script_run("packages/envoy-client/test/envoy-stack.ts",
                   'const build = Bun.spawn(["go", "build", "-o", `${bin}${path.sep}`, ...packages], {',
                   f"buildBinaries, run by {at(STALL, STALL_CLIENT)}, {at(STALL, 'Stall test of pi-envoy')} and "
                   f"{at(STALL, 'Stall test of claude-envoy')}", ENVOY, ["./internal/testnats/cmd/natsd", "./cmd/listener"], mode="build"),
    ], reads=reads(
        ("packages/envoy-client/**", f"{at(STALL, STALL_CLIENT)}: src/__tests__/nats-stall.test.ts and test/*.ts, which the other two import"),
        ("packages/pi-envoy/**", at(STALL, "Stall test of pi-envoy")),
        ("packages/claude-envoy/**", f"{at(STALL, 'Stall test of claude-envoy')} (tests/envoy-channel-stall.test.ts runs bin/envoy-channel.ts)"),
        ("packages/pi-shared/src/**", "packages/pi-envoy and packages/claude-envoy import @legion/pi-shared/*"),
        ("packages/contracts/**", at(STALL, "Build contracts for the stall suites")),
    ) + setup_go(STALL, f"{ENVOY}/go.mod") + bun_job(STALL) + CI_SELF),

    gate("go_notices", "lint", step=NOTICES_STEP, runs=go_notices_runs(), reads=reads(
        (NOTICES, f"{at('lint', NOTICES_STEP)} runs it"),
        (GO_TARGETS, f"{at('lint', NOTICES_STEP)}: jq reads it"),
        (CLIENT_TARGETS, f"{at('lint', NOTICES_STEP)}: jq reads it"),
        (INFRA_TARGETS, f"{at('lint', NOTICES_STEP)}: jq reads it"),
    ) + CI_SELF),

    # This check itself: it lists every module's packages (vet mode: each package directory, its
    # dependencies' and the test files, without the files tests read), reads the target lists
    # the runs above expand, and is its own script, engine, data and path model.
    gate("ci_filters", "ci-filters", runs=[
        run(DAEMON, ["./..."], f"{at('ci-filters', CHECK_STEP)}: go list over the daemon module", mode="vet", step=CHECK_STEP,
            text=".github/scripts/check-ci-filters.sh"),
        run(ENVOY, ["./..."], f"{at('ci-filters', CHECK_STEP)}: go list over the envoy module", mode="vet", step=CHECK_STEP,
            text=".github/scripts/check-ci-filters.sh"),
        run(INFRA, ["./..."], f"{at('ci-filters', CHECK_STEP)}: go list over the envoy-infra module", mode="vet", step=CHECK_STEP,
            text=".github/scripts/check-ci-filters.sh"),
    ], reads=reads(
        ("packages/pi-legion/scripts/skill-scenarios/daemon-pane.go", "the pi_legion gate's go run -overlay compiles it"),
        (".github/scripts/check-ci-filters.sh", at("ci-filters", CHECK_STEP)),
        (".github/scripts/ci_filter_coverage.py", "check-ci-filters.sh's engine"),
        (".github/scripts/ci_job_reads.py", "check-ci-filters.sh's data"),
        (".github/scripts/workflow_paths.py", "ci_filter_coverage.py reads filters through it"),
        (GO_TARGETS, "ci_job_reads.py expands the notices and pack runs over it"),
        (CLIENT_TARGETS, "ci_job_reads.py expands the notices runs over it"),
        (INFRA_TARGETS, "ci_job_reads.py expands the notices runs over it"),
        (DOCS_SITE, "ci_filter_coverage.py looks the docs gate's step citations up in it"),
    ) + reads(*((path, f"check-ci-filters.sh holds its on.push.paths equal to the {name} filter (PUSH_WORKFLOWS)")
                for name, path in PUSH_WORKFLOWS.items()))
      + setup_go("ci-filters", f"{DAEMON}/go.mod") + CI_SELF),

    # The ci-images job: the copy, and the package mode of the image gate, which reads ghcr.io.
    # These are the files that decide what those two steps do. The gate's tree mode is a step of
    # lint, which runs on every change, and no filter gates it.
    gate("ci_images", "ci-images", reads=reads(
        (".github/ci-images.yaml", f"{at('ci-images', COPY_STEP)} copies the rows it holds, and "
                                   f"{at('ci-images', PACKAGE_STEP)} reads each row's digest"),
        (".github/scripts/mirror-ci-images.sh", f"{at('ci-images', COPY_STEP)} runs it"),
        (".github/scripts/check-ci-images.sh", f"{at('ci-images', PACKAGE_STEP)} runs it"),
        (".github/scripts/ci_images.py", "both scripts read a row's upstream with it"),
        (".github/actions/install-crane/**", at("ci-images", "./.github/actions/install-crane")),
    ) + CI_SELF),
]

EXCLUDED = {
    "worker_image": "gates worker-image.yaml's image build; .github/scripts/check-image-trigger-paths.sh proves it covers "
                    "every file the build reads",
}
