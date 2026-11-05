#!/usr/bin/env bash
# Release source-state publisher, shared by the release workflows and by
# claude-envoy-rebuild.yaml, which is no release but pushes the built plugin to
# the claude-plugin branch with the same deploy key.
#
# The workflows split each release into a build job, which runs the
# dependencies' code and holds no credential, and a publish job, which holds
# the credentials and runs no dependency code (the rebuild workflow's push job
# is one). `version`, `check-built`, `extract-built`, `no-credential` and
# `fold-notes` are the build job's; `release`, `release-section`, `restart`,
# `push-tag` and `push-built` the publish job's; `tag`, `tagged`,
# `newest-version`, `handed` and `fold-plan` serve both.
#
# version:   the one version step every build job calls: print the release
#            this checkout gets, as the three lines `version=`, `tag=` and
#            `skip=` the caller appends to $GITHUB_OUTPUT (on skip, version
#            and tag are empty). A tag an earlier attempt pushed for this
#            checkout (`tagged`, below) is released again. Otherwise the
#            newest <tag-prefix> tag is the previous release, and
#            release-bump.sh, beside this script, computes the next version
#            from the commits since it that touch the <paths> after --, with
#            every --patch-path passed through. Without such a tag the release
#            line has not started, which is an error, unless --first-npm names
#            the npm package whose published version (0.0.0 when npm has none)
#            stands in for the tag, over the whole history. release-bump.sh's
#            `empty` (no commit in the paths) and `none` (no version-bumping
#            commit: a ::notice and a job-summary line) both give skip=true.
#            --expect <X.Y.Z> is for a publish job checking what its build job
#            handed it. When the tags now give another version, or nothing to
#            release, another run took the tag first: a ::notice says so, and
#            the command exits 3 when nothing of this checkout is left to
#            release (the tags give nothing to release, or another run tagged
#            this checkout itself) or 4 when the tags give a version for
#            commits no release holds yet, which a new run must build. No other
#            outcome exits 3 or 4. Every log line, notice and error goes to
#            stderr, so stdout holds the three lines on exit 0 and nothing
#            otherwise.
# restart:   what a publish job runs when `version --expect` exited 4: see to
#            it that a run of <workflow-file> on this run's branch
#            ($GITHUB_REF_NAME, which is main for every publish job) releases
#            this checkout, with $GH_TOKEN for gh. The branch is fetched right
#            after the runs are listed, so every listed run's commit is here. A
#            run of the workflow on the branch with a higher run number than
#            $GITHUB_RUN_NUMBER, not finished, whose commit holds HEAD, is
#            left to release it: it publishes a release holding HEAD, loses in
#            turn and runs this itself, or fails where a person sees it. A
#            ::notice names it and nothing is started. Only a later run counts,
#            so two runs of one commit never each leave the release to the
#            other. Otherwise a workflow_dispatch of <workflow-file> on the
#            branch with lost_races=<lost-races>+1 starts a run, which decides
#            the version again from the tags as they are then, and a ::notice
#            names the run URL gh printed. <lost-races> is how many runs
#            before this one each lost their version and started the next, a
#            count from 0 to 99 (anything else exits 2, starting nothing). At 3
#            nothing is started and the command fails with an ::error:: naming
#            the command a person runs: each loss means another run published
#            in between, so a chain that long means something keeps moving the
#            tags.
# tag:       create the release tag <tag> on $GITHUB_SHA, the commit the
#            release run started on, which main contains, as the legion-v,
#            legion-envoy-v and envoy-infra-v tags always named it; pushes
#            nothing (`push-tag` pushes) and commits nothing. Nothing of a
#            release is committed: the tags record every version, so no file
#            of this checkout is written and a package's committed manifest
#            keeps whatever version was last written to it. A release's build
#            job runs it too, before `fold-notes`: there the tag is local, no
#            step of that job pushes it, and it is what labels this release's
#            fragments in the plan fold-notes renders. <version> is the
#            release's version, X.Y.Z, and $GITHUB_SHA must be 40 lower-case
#            hex; either one refused is an ::error:: and exit 1. The tags are
#            fetched first, so a tag pushed since the checkout is seen. A tag
#            of that name already on $GITHUB_SHA is reused; one on any other
#            commit is an ::error:: naming both commits, and nothing is
#            tagged.
# tagged:    print the release tag an earlier attempt pushed for this
#            checkout, or nothing: the newest <tag-prefix> tag on HEAD itself,
#            the commit `tag` tags. A re-run of a release job checks out the
#            same commit, with every tag its first attempt pushed, and a
#            second run on the same commit sees the first run's tag; `version`
#            reads this to release that tag again, where computing the next
#            version from the tag would find an empty range and skip. The
#            version is the tag's name, which anyone who can push a tag
#            chooses, so that tag must be <tag-prefix>X.Y.Z, each field a
#            number without leading zeros: any other name fails with an
#            ::error:: on stderr naming it.
# newest-version: print the version of the newest <tag-prefix> tag, or nothing
#            when that tag's name is not <tag-prefix>X.Y.Z and when the
#            repository holds no such tag at all. The newest is the highest by
#            version order of the name (`--sort=-v:refname`), picked first and
#            checked after: a newest tag no release made prints nothing
#            instead of reaching past it to an older one. release.yaml reads
#            it for the version npm carries for a plugin that releases nothing
#            in a run, and `version` bumps from the same tag.
# release:   make the GitHub release of <tag>, in $GITHUB_REPOSITORY, carry
#            this run's <asset>s and be published. A release that does not
#            exist is created (`--verify-tag`, with generated notes, or the
#            --notes given). A published one is reused as it is, or, with
#            --upload-to-published, gets these assets over its own. A draft gets
#            these assets over its own (`--clobber`) and is then published. A
#            draft is one an earlier attempt's `gh release create` left between
#            creating and publishing it: gh deletes that draft itself when an
#            upload fails, so only a job cancelled or lost mid-upload leaves
#            one, possibly short of assets. A failed upload leaves the draft
#            unpublished; either failure fails the step, naming the release.
# release-section: write one section into the body of the GitHub release of
#            <tag>, in $GITHUB_REPOSITORY: the <heading> line, a blank line and
#            the bytes of <content-file>, which is '-' to read the content from
#            standard input. Every other line of the body gh prints is kept in
#            its order, and a section the body already holds under that heading
#            - that line, trailing blanks and a CR apart, and everything down to
#            the next heading of level one or two - is dropped before the new
#            one is appended. The write is a replace and not an append because
#            every caller is a step a re-run runs again: it renders the same
#            entries, or names the digest the version tag points at now, and an
#            append would leave the body holding that section twice. A body
#            already holding exactly what would be written, trailing newlines
#            apart, is left alone and nothing is edited; which of the two it did
#            is said on stdout. The heading must be a level-two heading line
#            ('## ' and a title), the one shape a later run finds again, and the
#            content must be a file that is not empty, or the heading would
#            stand over nothing: either is an ::error:: naming it, with nothing
#            written. It runs gh, awk and the shell alone, as every caller is a
#            job holding a credential.
# push-tag:  push the local tag <tag> to $GITHUB_REPOSITORY on GitHub over SSH
#            with the deploy key in $DEPLOY_KEY, for a job whose checkout keeps
#            no credential. The key and GitHub's host keys, pinned below, go
#            into a directory of its own that only the runner's user can read,
#            removed when the script exits, pushed or not (deploy_key_env,
#            which push-built shares). ssh reads no config file,
#            offers only that key, never prompts, and refuses any host key but
#            the pinned ones; GitHub publishes them at `gh api meta --jq
#            .ssh_keys`, which release-push.test.sh compares with these when
#            it can reach GitHub.
# check-built: check <tar>, a build of the Claude Envoy plugin, for what
#            push-built and extract-built accept, holding no credential and
#            writing nothing. packages/claude-envoy/scripts/verify-build.sh
#            runs it (that script's header names the jobs that run it), so a
#            pull request fails for whatever main's push job would refuse. <tar>
#            holds the package's dist/ and legion/ (`tar -cf <tar> -C
#            packages/claude-envoy dist legion`) and is checked from its listing:
#            every member a regular file or a directory (no symlink, no hard
#            link, nothing else); every name dist or legion, then path
#            components of letters, digits, '.', '_' and '-' only, so no
#            absolute path, no member outside the two directories, and no '..'
#            component; no regular member with an execute, setuid, setgid or
#            sticky bit; and the files every build holds present:
#            dist/envoy-channel.js, dist/session-hook.js,
#            dist/session-title-hook.js, dist/dispatch.js,
#            dist/THIRD_PARTY_NOTICES, legion/.claude-plugin/plugin.json and a
#            file under each of legion/skills/ and legion/agents/. Each refusal
#            is an ::error:: naming the member, and exit 1.
# extract-built: put <tar>, a build of the Claude Envoy plugin, into this
#            checkout, holding no credential. The tar is checked as
#            check-built checks it, each refusal an ::error:: naming the
#            member with nothing changed. Then packages/claude-envoy/dist and
#            packages/claude-envoy/legion under the repository's root (`git
#            rev-parse --show-toplevel`) are replaced by the tar's content, so
#            a file the tar lacks is deleted; a failed extraction is an
#            ::error::. The rebuild's build job runs it to compare the build
#            with the files main tracks, and ci.yaml's claude-envoy job to
#            pack the build, each in its own disposable checkout; push-built
#            does the same before it commits. Nothing a person runs locally
#            writes the two directories.
# push-built: land a build of the Claude Envoy plugin on the claude-plugin
#            branch, for claude-envoy-rebuild.yaml's push job. <tar> is what
#            its build job built from <base-sha>, main's tip when that job
#            started: the package's dist/ and legion/. The branch is the one
#            .claude-plugin/marketplace.json names as the plugin's source, and
#            no workflow of this repository runs on a push to it, so the merge
#            queue is left alone; main keeps the dist/ and legion/ files it
#            already tracks, which nothing here rewrites. Every repository path
#            push-built reads is the root's, so it moves to the repository's
#            root (`git rev-parse --show-toplevel`) first, with <tar> made
#            absolute before the move, since that path is the caller's: a run
#            from a subdirectory of the checkout lands the same commit. HEAD
#            must be <base-sha>, 40 lower-case hex, and the work tree clean,
#            which is read from the root too. The tar is then checked as
#            check-built checks it, before anything reaches GitHub, so a tar
#            that fails a check is refused however main stands. Each of these
#            refusals is an ::error:: and exit 1 that pushes nothing and starts
#            no run.
#            push-built then fetches main and the branch once with the deploy
#            key (deploy_key_env, as push-tag does); a branch the remote does
#            not hold yet is no failure and holds no build. The push job waits
#            for a runner after the build job, often longer than main goes
#            between merges, so main has usually moved past the base by then.
#            The build is made on main's tip and lands on the branch all the
#            same, unless one of two rules stops it. When main's tip does not
#            descend from the base, main was rewritten: nothing is pushed, a
#            fresh run of the workflow on main (`gh workflow run`, with
#            $GH_TOKEN) builds the new tip, and a ::notice names both commits.
#            When the branch already holds a newer build, nothing is pushed, no
#            run is started, and a ::notice says so. Every commit of the branch
#            is a build, so the branch's build is its tip. When
#            github-actions[bot] authored the tip with exactly the subject
#            below, and the 12 hex in that subject name a commit that is its
#            ancestor, it is a build of that commit. Otherwise (a subject a
#            person wrote) it is a build of at most the tip itself. This build
#            is the newer one when that commit is the base or an ancestor of
#            it, and when the branch holds no commit at all.
#
#            The build is then put into the checkout as extract-built puts it,
#            and the two directories are written over main's tip's tree in a
#            temporary index, so the commit carries main's whole tree with the
#            build over it. The tip is never checked out: it may carry another
#            release-push.sh, and a checkout would rewrite this script while
#            bash runs it. A build the branch's tip already holds, tree for
#            tree, stages nothing: a ::notice names the branch and nothing is
#            pushed. Main's own files are not that test: main's copy of the two
#            directories is frozen at the last build it carried, so a build
#            equal to it is committed all the same, and a branch the remote
#            lacks holds no build at all, so the first run of all commits too.
#            Otherwise github-actions[bot], as `tag` commits, commits the tree
#            on main's tip as `chore(claude-envoy): rebuild dist/ and legion/
#            from <the base's first 12 hex>`. The subject names the base, the
#            commit the build was made from, which the newer rule above reads.
#            It carries no skip keyword and needs none: every workflow of this
#            repository that triggers on a push names `branches: [main]`, so a
#            push to the branch starts no run (the build job ran the
#            private-name check over the bundle already). When main had moved,
#            a ::notice titled "Landing on claude-plugin" names the base, the
#            tip and the commit, which changes only the two directories. The
#            commit lands by land_on_plugin_branch (below) on the branch, over
#            whatever it held.
#
#            A build of the base made on a newer tip leaves the branch's build
#            one run behind its inputs when a build input changed between the
#            two. The inputs are the git pathspecs in
#            packages/claude-envoy/scripts/build-inputs.txt, one per line (an
#            empty line or one starting # is ignored), read from the base with
#            git and compared between the base and main's tip with `git diff
#            --no-renames`, so a renamed file counts by its old path and its
#            new one alike. Main is what that question is asked of, whatever
#            the branch holds: a source file changed since the build is the one
#            real staleness reason. A base without the list, or with no
#            pathspec line in it, counts as changed, with a ::warning naming
#            the file. When an input changed, a fresh run of the workflow on
#            main is started once the build has landed, or when the branch's
#            tip already holds the build, and a ::notice names the first
#            changed paths and the run gh printed. A failed fresh run is an
#            ::error:: and exit 1 that names the command a person runs; after a
#            landing, it says the build already landed.
#
#            Main can move again between push-built's fetch and the push.
#            land_on_plugin_branch fetches main again and pushes only when
#            main's tip is still the tip the commit was made on, so nothing
#            lands that was built on a tip push-built did not check. Otherwise
#            land_on_plugin_branch starts the one fresh run itself, and
#            push-built starts no second one, whether or not an input changed.
#            There is no retry loop: the fresh run is the retry.
#
#            land_on_plugin_branch <onto-sha> <workflow-file>, which push-built
#            ends with, lands HEAD on $plugin_branch and never rebases, so
#            push-built exits 0 whenever it returns (unless the fresh run it
#            then starts for a changed build input fails). It
#            refuses a HEAD that is not exactly one commit whose one parent is
#            <onto-sha>, which refuses any <onto-sha> but that parent's full
#            sha (push-built has refused a base that is not 40 lower-case hex
#            already), then fetches main again with the deploy key
#            (deploy_key_env, as push-tag does): HEAD was made on main's tip,
#            whichever branch it goes to, so main's tip is what decides. When
#            main's tip is <onto-sha> it force-updates the branch to HEAD. The
#            force is needed because the branch's tip is the previous
#            rebuild's commit and never an ancestor of this one, so no push of
#            it is a fast-forward. When main's tip is another commit, before
#            the push or after a rejected push, it pushes nothing: it starts a
#            fresh run of <workflow-file> on main (`gh workflow run`, with
#            $GH_TOKEN), which builds main's tip, and a ::notice names both
#            commits. In both cases it returns 0, and leaves which it did in
#            $landed (pushed or restarted), so push-built starts no second run
#            after land_on_plugin_branch started one. A failed fetch, a push
#            rejected while main's tip is still <onto-sha>, or a failed `gh
#            workflow run` is an ::error:: and exit 1.
# handed:    check what a build job handed a publish job before the publish job
#            acts on it, and print the release tag, <tag-prefix><version>. The
#            version is the build job's output, so it must be X.Y.Z, each field
#            a number without leading zeros. <dir> must hold exactly the
#            <file>s, each a regular file (a symlink is not), and nothing else
#            at any depth. The caller builds the <file> names from the version
#            and its own checkout, so the build job chooses the files' bytes but
#            not their names; what the publish job writes into <dir> itself,
#            such as SHA256SUMS, it writes after this check. Every refusal is an
#            ::error:: on stderr naming what is wrong, and prints no tag.
# no-credential: the tripwire a build job runs last: fail when a credential is
#            reachable from the job. It prints presence and names, never a
#            value: git configuration matching credential, insteadOf,
#            sshCommand or extraHeader (names only), what ~/.ssh holds beyond
#            known_hosts, and whether GIT_SSH_COMMAND, SSH_AUTH_SOCK,
#            DEPLOY_KEY or the OIDC token variables are set. It is a check
#            over configuration, not a sandbox: dependency code that already
#            ran in the job could have planted a `git` for this very step, so
#            the build/publish split is the boundary and this only catches a
#            credential wired into a build job by mistake.
# fold-plan: print which release each changelog fragment of the <spec>s
#            belongs to, one tab-separated line per fragment:
#            <package-dir>, <label>, <date> (YYYY-MM-DD, UTC), <path>, sorted by
#            package in the order of the specs, then by label as versions
#            (`sort -V`), then by path; nothing printed means no fragment of
#            the specs is in a release yet. fold-notes renders the groups of
#            this plan, one of which is a release's own entries.
#            Each <spec> is <package-dir>[=<tag-prefix>], and the package must
#            hold a CHANGELOG.md at HEAD. The fragments are the entries directly
#            in <package-dir>/.changes/unreleased/ that HEAD holds (git's tree,
#            never the working tree). Each must be a regular file (mode 100644
#            or 100755) whose name ends in .yaml: any other entry there, such as
#            a subdirectory, a symlink, a submodule or a .yml file, is in no
#            plan and is rendered nowhere, so fold-plan fails with one
#            ::error:: that names every such entry of every spec, sorted, and
#            tells the author to move or rename a fragment in one commit with
#            `git mv`, and prints no plan.
#            A file under the .changes/ of a package no <spec> names is not
#            refused: a refusal would leave the specs' own fragments unplanned
#            with it. One ::warning:: on stderr names every such path HEAD's
#            tree holds, sorted, and the specs it was given; the plan is
#            printed and the command exits 0 all the same. The warning says to
#            remove the file, or to give its package a .changie.yaml and a
#            CHANGELOG.md and add it to .github/changelog-packages.
#            fold-orphans below is that check made strict, and ci.yaml's lint
#            job runs it on every pull request, so such a file is caught
#            before it reaches main; fold-notes makes neither check, since it
#            renders one release's group of the plan of that release's own
#            package.
#            With a tag prefix (a package with releases of its own), a
#            fragment's label is X.Y.Z of the lowest <tag-prefix>X.Y.Z tag
#            that contains the commit which first added the entry, following
#            renames: the release that first shipped the entry, whose tag
#            sits on the commit the release run built; other names under the
#            prefix are not releases and are skipped. A commit that
#            deletes a file and adds one git pairs with it as a rename, as
#            `git mv` does, renames the entry; a fragment deleted in one commit
#            and added again in another is a new entry, and a copy is never
#            followed. A fragment no such tag contains is in no release yet,
#            and is unprinted until one ships it.
#            The date is the tag's commit's committer date. Without a prefix (a
#            package with no release of its own), every fragment's label is
#            main@<HEAD's first 12 hex digits>, dated by HEAD's committer date.
#            A fragment's path is one file to git, never a pattern: a name
#            holding a glob character gets the commit that added that file. It
#            reads only git, so it computes the same plan from the same
#            checkout and tags.
# fold-orphans: fail when HEAD's tree holds a file under the .changes/ of a
#            package no <spec> names, the strict form of the warning fold-plan
#            gives: one ::error:: naming every such path, sorted, the specs it
#            was given and what to do (remove the file, or give its package a
#            .changie.yaml and a CHANGELOG.md and add it to
#            .github/changelog-packages), and exit 1; nothing is printed and
#            the exit is 0 when HEAD holds no such file. It reads HEAD's tree
#            alone - no history, no tag, no working tree, no CHANGELOG.md - so
#            a shallow checkout without the tags is enough for it. ci.yaml's
#            lint job runs it over .github/changelog-packages on every pull
#            request, inside a required check: a fragment of a package no
#            release and no documentation page renders is refused there, while
#            fold-plan only warns about it. Each <spec> is
#            <package-dir>[=<tag-prefix>], as fold-plan's is, and only the
#            <package-dir> counts; no spec at all, or one of another shape, is
#            refused as fold-plan refuses it.
# fold-notes: render changelog entries with changie, which is dependency code,
#            into <notes-dir>, which must be absent or empty and is created.
#            Each (package, label) of fold-plan's plan gets a directory of its
#            own holding a copy of the package's .changie.yaml and only that
#            group's fragments as HEAD holds them, where `changie batch 0.0.0`
#            renders them, and the rendered notes become
#            <notes-dir>/<package basename>_<label>.md. --label <label> narrows
#            the plan to the groups labelled <label> before anything is
#            rendered: a release's build job renders the entries of its own
#            release, and every fragment a release ever shipped stays under
#            .changes/unreleased/, so rendering the whole plan would run one
#            changie per release ever made. A plan left with no group writes
#            nothing and exits 0, as an empty plan does. A fragment changie
#            refuses fails the command with an ::error:: naming the package,
#            the label, each fragment changie refuses on its own and changie's
#            output.
set -euo pipefail

command=${1:?usage: release-push.sh version <tag-prefix> [--first-npm <npm package>] [--expect <X.Y.Z>] [--patch-path <path>]... -- <paths>... | release-push.sh restart <workflow-file> <lost-races> | release-push.sh tag <version> <tag> | release-push.sh tagged <tag-prefix> | release-push.sh newest-version <tag-prefix> | release-push.sh release [--notes <text>] [--upload-to-published] <tag> <title> <asset>... | release-push.sh release-section <tag> <heading> <content-file> | release-push.sh push-tag <tag> | release-push.sh check-built <tar> | release-push.sh extract-built <tar> | release-push.sh push-built <base-sha> <tar> | release-push.sh handed <tag-prefix> <version> <dir> <file>... | release-push.sh fold-plan <spec>... | release-push.sh fold-orphans <spec>... | release-push.sh fold-notes [--label <label>] <notes-dir> <spec>... | release-push.sh no-credential}

# The identity of the commit this script makes, push-built's rebuild: GitHub
# Actions' own bot.
bot_name='github-actions[bot]'
bot_email='41898282+github-actions[bot]@users.noreply.github.com'

# The git pathspecs of every file that can change the Claude Envoy plugin's
# build, one per line, which push-built reads from the build's base with git.
build_inputs=packages/claude-envoy/scripts/build-inputs.txt

# The branch push-built force-updates with the built Claude Envoy plugin, which
# .claude-plugin/marketplace.json names as the source Claude Code installs the
# plugin from. Nothing on main reads it, so a rebuild push leaves the merge
# queue alone; packages/claude-envoy/tests/envoy-channel-server.test.ts reads
# this line and holds the marketplace entry's ref equal to it.
plugin_branch=claude-plugin

# is_plain_version <value>: true when <value> is X.Y.Z, each field a number
# without leading zeros (release-bump.sh does arithmetic on the fields, where a
# leading zero reads as octal).
is_plain_version() { [[ $1 =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; }

# is_commit_sha <value>: true when <value> is a full commit sha, 40 lower-case
# hex.
is_commit_sha() { [[ $1 =~ ^[0-9a-f]{40}$ ]]; }

# quote_all <value>...: the values, each %q-quoted, separated by spaces.
quote_all() {
  local quoted
  printf -v quoted '%q ' "$@"
  printf '%s' "${quoted% }"
}

# github_known_hosts: GitHub's SSH host keys, the only ones deploy_key_env accepts.
github_known_hosts() {
  cat <<'KNOWN_HOSTS'
github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl
github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=
github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=
KNOWN_HOSTS
}

# fail_error <message>: fail the command with an ::error:: on stderr naming it.
fail_error() {
  echo "::error::release-push.sh $command: $1" >&2
  exit 1
}

# on_exit_remove <dir>: remove <dir> when the script exits, together with every
# other directory given here, under the one EXIT trap they share (push-built
# makes the key directory and a temporary index's directory).
exit_removes=()
on_exit_remove() {
  exit_removes+=("$1")
  trap 'rm -rf "${exit_removes[@]}"' EXIT
}

# deploy_key_env's directory, empty until it made one.
ssh_dir=''

# deploy_key_env: make this process's git reach GitHub over SSH with
# the deploy key in $DEPLOY_KEY (the header's push-tag paragraph): the key and
# GitHub's host keys go into a directory only the runner's user can read,
# removed when the script exits, GIT_SSH_COMMAND points ssh at them and nothing
# else, and $remote is the repository's SSH URL on GitHub. Before anything is
# made, an unset or empty $DEPLOY_KEY or $GITHUB_REPOSITORY is an ::error::
# naming it. A second call returns at once: push-built calls it before its own
# fetch and land_on_plugin_branch again, and the first call's directory serves
# both.
deploy_key_env() {
  local key known_hosts
  [ -z "$ssh_dir" ] || return 0
  [ -n "${DEPLOY_KEY:-}" ] || fail_error "DEPLOY_KEY holds the deploy key to reach GitHub with; it is unset or empty."
  [ -n "${GITHUB_REPOSITORY:-}" ] || fail_error "GITHUB_REPOSITORY names the repository on GitHub; it is unset or empty."
  ssh_dir=$(mktemp -d)
  on_exit_remove "$ssh_dir"
  (
    umask 077
    printf '%s\n' "$DEPLOY_KEY" > "$ssh_dir/key"
    github_known_hosts > "$ssh_dir/known_hosts"
  )
  printf -v key '%q' "$ssh_dir/key"
  printf -v known_hosts '%q' "$ssh_dir/known_hosts"
  export GIT_SSH_COMMAND="ssh -F /dev/null -i $key -o IdentitiesOnly=yes -o BatchMode=yes -o UserKnownHostsFile=$known_hosts -o GlobalKnownHostsFile=/dev/null -o StrictHostKeyChecking=yes"
  remote="git@github.com:$GITHUB_REPOSITORY.git"
}

# is_ancestor <a> <b>: true when commit <a> is <b> or an ancestor of it. A
# failure of git itself, not an answer, is an ::error:: and exit 1.
is_ancestor() {
  local status=0
  git merge-base --is-ancestor "$1" "$2" || status=$?
  [ "$status" -le 1 ] || fail_error "checking whether $1 is an ancestor of $2 failed"
  return "$status"
}

# fresh_run <workflow-file> <why> <title> <lead>: start a run of <workflow-file>
# on main (`gh workflow run`, with $GH_TOKEN), then give a ::notice titled
# <title>: <lead>, then the run's URL gh printed, or "the newest run of
# <workflow-file>" on an older gh that prints none (restart's note). A failed
# start is an ::error:: and exit 1 that says <why> and names the command a
# person runs instead.
fresh_run() {
  local workflow=$1 why=$2 title=$3 lead=$4 started
  started=$(gh workflow run "$workflow" --repo "$GITHUB_REPOSITORY" --ref main) ||
    fail_error "$why, and starting a fresh run of $workflow on main failed; start one by hand: gh workflow run $workflow --repo $GITHUB_REPOSITORY --ref main"
  echo "::notice title=$title::$lead a fresh run of $workflow on main, ${started:-the newest run of $workflow}, builds main's tip" >&2
}

# newest_tag <tag-prefix>: print the newest <tag-prefix> tag, the highest by
# version order of its whole name, or nothing when there is none. The name is
# whatever its pusher chose: `newest-version` prints the version of this tag
# when it is <tag-prefix>X.Y.Z, and `version` bumps from it.
newest_tag() {
  git for-each-ref --count=1 --sort=-v:refname --format='%(refname:strip=2)' "refs/tags/$1*"
}

# pushed_tag <tag-prefix>: print the release tag an earlier attempt pushed for
# this checkout, or nothing (the header's `tagged` paragraph). A tag that is
# not <tag-prefix>X.Y.Z fails with an ::error::.
pushed_tag() {
  local prefix=$1 head tags candidate shown
  head=$(git rev-parse HEAD)
  # The prefix's tags on HEAD and its descendants, newest version first. Ref
  # names hold no whitespace, so the list splits into one tag per word.
  tags=$(git tag --list "$prefix*" --contains HEAD --sort=-v:refname)
  for candidate in $tags; do
    if [ "$(git rev-parse "$candidate^{commit}")" = "$head" ]; then
      if ! is_plain_version "${candidate#"$prefix"}"; then
        printf -v shown '%q' "$candidate"
        echo "::error::release-push.sh $command: $shown, the release tag for this checkout, is not ${prefix}X.Y.Z; refusing it" >&2
        return 1
      fi
      echo "$candidate"
      return 0
    fi
  done
}

# check_built <tar>: refuse <tar> unless it is a build push-built accepts (the
# header's check-built paragraph), reading only its listing; every refusal is an
# ::error:: naming the member, and exit 1.
check_built() {
  local tar_file=$1 shown_tar names listing i member mode shown need dir found
  local -a members modes
  local -A regular=()
  shown_tar=$(quote_all "$tar_file")
  # Every member's name from -t, one per line (GNU tar escapes a newline in a
  # name), and its type and mode from the first field of the same line of -tv.
  names=$(tar -tf "$tar_file") || fail_error "listing $shown_tar failed"
  listing=$(tar -tvf "$tar_file") || fail_error "listing $shown_tar failed"
  [ -n "$names" ] || fail_error "$shown_tar holds no member"
  mapfile -t members <<<"$names"
  mapfile -t modes <<<"$listing"
  if [ "${#members[@]}" != "${#modes[@]}" ]; then
    fail_error "$shown_tar lists ${#members[@]} names but ${#modes[@]} entries; refusing it"
  fi
  for i in "${!members[@]}"; do
    member=${members[i]} mode=${modes[i]%% *}
    shown="member $(quote_all "$member") of $shown_tar"
    case $mode in
      -* | d*) ;;
      l*) fail_error "$shown is a symlink; only regular files and directories are accepted" ;;
      h*) fail_error "$shown is a hard link; only regular files and directories are accepted" ;;
      *) fail_error "$shown is of type '${mode:0:1}', not a regular file or a directory" ;;
    esac
    if [[ $member == /* ]]; then
      fail_error "$shown is an absolute path"
    fi
    if [[ /$member/ == */../* ]]; then
      fail_error "$shown has a '..' component"
    fi
    if ! [[ $member =~ ^(dist|legion)(/[A-Za-z0-9._-]+)*/?$ ]]; then
      fail_error "$shown is not dist, legion or a path under them in letters, digits, '.', '_' and '-'"
    fi
    if [[ $mode == -* ]]; then
      # The owner's, the group's and the others' execute places of the mode.
      if [ "${mode:3:1}${mode:6:1}${mode:9:1}" != --- ]; then
        fail_error "$shown has the mode $mode, with an execute, setuid, setgid or sticky bit; no file of the plugin is executable"
      fi
      regular[$member]=1
    fi
  done
  for need in dist/envoy-channel.js dist/session-hook.js dist/session-title-hook.js dist/dispatch.js \
    dist/THIRD_PARTY_NOTICES legion/.claude-plugin/plugin.json; do
    if [ -z "${regular[$need]:-}" ]; then
      fail_error "$shown_tar lacks $need, a file every build of the plugin holds"
    fi
  done
  for dir in legion/skills legion/agents; do
    found=false
    for member in "${!regular[@]}"; do
      if [[ $member == "$dir"/* ]]; then found=true; break; fi
    done
    if ! "$found"; then
      fail_error "$shown_tar holds no file under $dir/, which every build of the plugin fills"
    fi
  done
}

# extract_built <tar>: move to the repository's root, check <tar> as
# check_built does, and replace packages/claude-envoy/dist and
# packages/claude-envoy/legion with its content (the header's extract-built
# paragraph). The command goes on from the root.
extract_built() {
  local tar_file=$1 root shown_tar
  # The tar's path is made absolute before the move to the checkout's root,
  # where the package's paths below are read.
  case $tar_file in /*) ;; *) tar_file=$PWD/$tar_file ;; esac
  shown_tar=$(quote_all "$tar_file")
  root=$(git rev-parse --show-toplevel) || fail_error "finding the repository's root failed"
  cd "$root"
  check_built "$tar_file"
  # The tar is the whole of both directories: a file it lacks is deleted.
  rm -rf packages/claude-envoy/dist packages/claude-envoy/legion
  tar -xf "$tar_file" -C packages/claude-envoy --no-same-owner || fail_error "extracting $shown_tar into packages/claude-envoy failed"
}

# commit_build_on <tip> <base> <branch-tip>: commit the build extract_built just
# wrote over <tip>'s tree, as github-actions[bot] with the subject naming
# <base>, the commit the build was made from, and move HEAD to it, leaving the
# commit in $commit, or $commit empty when the plugin branch's tip
# <branch-tip> already holds exactly this tree, where nothing is committed (the
# out-parameter build_inputs_changed sets inputs_why by). An empty <branch-tip>
# is a branch the remote does not hold, which holds no build, so there is
# always something to commit. The commit is not printed, because
# `commit=$(commit_build_on ...)` would run it in a subshell, whose EXIT trap
# removes every on_exit_remove directory, the deploy key's among them, before
# land_on_plugin_branch pushes. The command runs from the repository's root,
# where extract_built left it.
commit_build_on() {
  local tip=$1 base=$2 branch_tip=$3 index_dir tree branch_tree
  commit=''
  # The two directories are written over the tip's tree in an index of its
  # own, and the tip is never checked out: it may carry another
  # release-push.sh, and a checkout would rewrite this script while bash runs
  # it. Only the two directories can change: the index starts as the tip's
  # tree, and `git add` is limited to them. The commit's parent is the tip, so
  # it carries main's whole tree with the build over it.
  index_dir=$(mktemp -d) || fail_error "making a directory for a temporary index failed"
  on_exit_remove "$index_dir"
  GIT_INDEX_FILE="$index_dir/index" git read-tree "$tip" || fail_error "reading $tip's tree into a temporary index failed"
  GIT_INDEX_FILE="$index_dir/index" git add -A -- packages/claude-envoy/dist packages/claude-envoy/legion ||
    fail_error "staging packages/claude-envoy/dist and packages/claude-envoy/legion over $tip's tree failed"
  tree=$(GIT_INDEX_FILE="$index_dir/index" git write-tree) || fail_error "writing the build's tree failed"
  rm -rf "$index_dir"
  # The tree is compared with the branch's, never with main's: main's copy of
  # the two directories is frozen at the last build it carried, so a build equal
  # to main's files is still the one the branch must hold.
  if [ -n "$branch_tip" ]; then
    branch_tree=$(git rev-parse "$branch_tip^{tree}") || fail_error "reading $branch_tip's tree failed"
    [ "$tree" != "$branch_tree" ] || return 0
  fi
  # The identity `tag` commits as, set for author and committer alike in the
  # environment, which wins over any GIT_AUTHOR_* or GIT_COMMITTER_* already
  # set. The subject names the base, the commit the build was made from, which
  # branch_build_base reads back.
  commit=$(GIT_AUTHOR_NAME=$bot_name GIT_AUTHOR_EMAIL=$bot_email GIT_COMMITTER_NAME=$bot_name GIT_COMMITTER_EMAIL=$bot_email \
    git commit-tree --no-gpg-sign -p "$tip" -m "chore(claude-envoy): rebuild dist/ and legion/ from ${base:0:12}" "$tree") ||
    fail_error "committing the build on $tip failed"
  git update-ref --no-deref HEAD "$commit" || fail_error "moving HEAD to $commit failed"
}

# branch_build_base <tip>: print the commit the build the plugin branch holds at
# <tip> was made from, or nothing when <tip> is empty, a branch the remote does
# not hold at all and so no recorded build (the header's push-built paragraph).
# Every commit of the branch is a build, so the tip itself is the one to read:
# when github-actions[bot] authored it with exactly commit_build_on's subject,
# and the 12 hex there name a commit that is its ancestor, that commit is
# printed; otherwise (a subject a person wrote) the tip itself, as a build of at
# most that commit.
branch_build_base() {
  local tip=$1 author subject named
  local rebuild_re='^chore\(claude-envoy\): rebuild dist/ and legion/ from ([0-9a-f]{12})$'
  [ -n "$tip" ] || return 0
  author=$(git log -1 --format=%ae "$tip") || fail_error "reading the author of $tip failed"
  subject=$(git log -1 --format=%s "$tip") || fail_error "reading the subject of $tip failed"
  if [ "$author" = "$bot_email" ] && [[ $subject =~ $rebuild_re ]] &&
    named=$(git rev-parse --verify --quiet "${BASH_REMATCH[1]}^{commit}") &&
    is_ancestor "$named" "$tip"; then
    echo "$named"
  else
    echo "$tip"
  fi
}

# build_inputs_changed <base> <tip>: set inputs_why to why a build input changed
# between <base> and <tip>, naming the first changed paths, or to nothing when
# none did (the header's push-built paragraph). Both are commits of main - the
# build's base and main's fetched tip - however far the plugin branch the commit
# goes to stands behind them: a source file changed since the build is the one
# reason the build is stale. The pathspecs are $build_inputs as <base> holds it,
# read with git; a <base> without that file, or with no pathspec line in it,
# counts as changed, with a ::warning naming it.
build_inputs_changed() {
  local base=$1 tip=$2 list_text line
  local -a pathspecs=() changed=()
  inputs_why=''
  if git cat-file -e "$base:$build_inputs" 2>/dev/null; then
    list_text=$(git show "$base:$build_inputs") || fail_error "reading $build_inputs at $base failed"
    while IFS= read -r line; do
      [ -z "$line" ] || [[ $line == '#'* ]] || pathspecs+=("$line")
    done <<<"$list_text"
  fi
  if [ ${#pathspecs[@]} -eq 0 ]; then
    inputs_why="$base holds no pathspec in $build_inputs, the list of the build's inputs, so every change since it counts as a build input"
    echo "::warning title=No build input list::$inputs_why" >&2
    return 0
  fi
  # --no-renames: a renamed file is listed by its old path and its new one, as
  # a deletion and an addition, whatever git's rename configuration says.
  mapfile -d '' -t changed < <(git diff -z --no-renames --name-only "$base" "$tip" -- "${pathspecs[@]}")
  wait "$!" || fail_error "comparing the build inputs ($build_inputs) between $base and $tip failed"
  if [ ${#changed[@]} -gt 0 ]; then
    inputs_why="build inputs changed between $base and $tip: $(quote_all "${changed[@]:0:3}")"
    [ ${#changed[@]} -le 3 ] || inputs_why+=" and $((${#changed[@]} - 3)) more"
  fi
}

# version_gt <a> <b>: true when version <a> is higher than <b> (`sort -V`).
version_gt() { [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n 1)" = "$1" ]; }

# check_hand_over <dir> <what> <file>...: refuse a <dir> that does not hold
# exactly the <file>s, each a regular file, and nothing else at any depth (the
# header's handed paragraph), with an ::error:: on stderr for each fault; an
# entry no <file> names is reported as what <what>. Exits 1 on a refusal.
check_hand_over() {
  local dir=$1 what=$2 file entry listed refused=false missing=() irregular=() unlisted=()
  shift 2
  for file in "$@"; do
    if [ -z "$file" ] || [ "$file" = . ] || [ "$file" = .. ] || [[ $file == */* ]]; then
      fail_error "$(quote_all "$file") is not a file name: each <file> is a name in <dir>, with no /"
    fi
  done
  if [ ! -d "$dir" ] || [ -L "$dir" ]; then
    fail_error "$(quote_all "$dir") is not a directory"
  fi

  for file in "$@"; do
    if [ -L "$dir/$file" ] || { [ -e "$dir/$file" ] && [ ! -f "$dir/$file" ]; }; then
      irregular+=("$file")
    elif [ ! -e "$dir/$file" ]; then
      missing+=("$file")
    fi
  done
  # Everything under <dir>, at any depth, sorted so a refusal names it the same
  # way every time. Read through a process substitution, not a temporary file,
  # so nothing is left to remove and no EXIT trap is needed; wait gives the
  # status of find and sort (pipefail holds in the substitution too).
  while IFS= read -r -d '' entry; do
    entry=${entry#"$dir"/}
    listed=false
    for file in "$@"; do
      [ "$file" != "$entry" ] || listed=true
    done
    "$listed" || unlisted+=("$entry")
  done < <(find "$dir" -mindepth 1 -print0 | sort -z)
  wait "$!" || fail_error "listing $(quote_all "$dir") failed"

  if [ ${#missing[@]} -gt 0 ]; then
    echo "::error::release-push.sh $command: $(quote_all "$dir") lacks $(quote_all "${missing[@]}")" >&2
    refused=true
  fi
  for file in "${irregular[@]}"; do
    echo "::error::release-push.sh $command: $(quote_all "$file") in $(quote_all "$dir") is not a regular file" >&2
    refused=true
  done
  if [ ${#unlisted[@]} -gt 0 ]; then
    echo "::error::release-push.sh $command: $(quote_all "$dir") holds what $what: $(quote_all "${unlisted[@]}")" >&2
    refused=true
  fi
  if "$refused"; then
    exit 1
  fi
}

# commit_date <rev>: the UTC date, YYYY-MM-DD, of <rev>'s commit.
commit_date() { TZ=UTC git log -1 --date=format-local:%Y-%m-%d --format=%cd "$1"; }

# first_added <path>: print the commit that first added <path>, a file HEAD
# holds, following renames. The commit that gave the file its name is the one
# that added <path>; when that commit also deleted a file git pairs with it as a
# rename (diff-tree -M, a deleted file and an added one alike enough), the file
# had another name before, and the search goes on for that name from the
# commit's parent. A copy is not followed: `git log --follow` follows copies too
# (it finds them among every file the parent holds), and fragments are alike
# enough that a new one reads as a copy of an older one, so it would give the
# new one the older one's commit. Every name is read literally.
first_added() {
  local path=$1 rev=HEAD commit status from to renamed
  while :; do
    commit=$(git --literal-pathspecs log -1 --diff-filter=A --format=%H "$rev" -- "$path")
    renamed=''
    while IFS= read -r -d '' status; do
      if [[ $status == R* ]]; then
        IFS= read -r -d '' from && IFS= read -r -d '' to
        [[ $to != "$path" ]] || renamed=$from
      else
        IFS= read -r -d '' to
      fi
    done < <(git diff-tree -r -z -M --root --no-commit-id --name-status "$commit")
    [ -n "$renamed" ] || break
    path=$renamed rev=$commit^
  done
  printf '%s\n' "$commit"
}

# A package spec, which fold-plan, fold-orphans and fold-notes all
# take: <package-dir>[=<tag-prefix>]. The package dir goes into paths and
# pathspecs, the prefix into a tag pattern, and the dir's basename names the
# notes file of each group. package_dir_re keeps the dir free of pathspec magic
# (a leading ':') and of glob characters, so it needs no --literal-pathspecs; a
# fragment's name is anything git holds, so each git call given one reads it
# literally.
package_dir_re='^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$'

# check_specs <spec>...: fail unless at least one <spec> is given and each is
# <package-dir>[=<tag-prefix>]. fold_plan checks this and more of every spec (a
# repeated basename, a package holding no CHANGELOG.md); fold-orphans, which
# reads the <package-dir>s and nothing else, checks this alone.
check_specs() {
  local spec dir prefix
  [ $# -gt 0 ] || fail_error "no package spec given; each is <package-dir>[=<tag-prefix>]"
  for spec in "$@"; do
    dir=${spec%%=*} prefix=''
    [[ $spec != *=* ]] || prefix=${spec#*=}
    if ! [[ $dir =~ $package_dir_re ]] || { [[ $spec == *=* ]] && ! [[ $prefix =~ ^[A-Za-z0-9._-]+$ ]]; }; then
      fail_error "package spec $(quote_all "$spec") is not <package-dir>[=<tag-prefix>], a relative path of letters, digits, '.', '_' and '-' and a prefix of the same without '/'"
    fi
  done
}

# group_commits <tag-prefix> <label>: set group_tag to the tag
# <tag-prefix><label> and in_group to the commits it contains that no lower
# <tag-prefix>X.Y.Z tag contains, one key each; true when there is such a tag,
# false when <label> is no X.Y.Z or the repository holds no such tag, where no
# fragment carries the label and the group is empty.
#
# Those commits are exactly the ones whose lowest tag of the prefix is this
# label, which is the rule fold_plan applies to the commit that first added a
# fragment. Reading them in one `git rev-list` is what keeps a release's build
# job from walking every tag of the prefix once per fragment, and every
# fragment a release ever shipped stays under .changes/unreleased/.
group_commits() {
  local prefix=$1 label=$2 candidate version commit
  local -a lower=()
  group_tag=$prefix$label
  in_group=()
  is_plain_version "$label" || return 1
  git rev-parse -q --verify "refs/tags/$group_tag^{commit}" >/dev/null || return 1
  for candidate in $(git tag --list "$prefix*"); do
    version=${candidate#"$prefix"}
    is_plain_version "$version" || continue
    version_gt "$label" "$version" || continue
    lower+=("$candidate")
  done
  while IFS= read -r commit; do
    in_group[$commit]=1
  done < <(git rev-list "$group_tag" --not "${lower[@]}")
}

# fold_plan <label> <spec>...: set plan to the lines fold-plan prints (the
# header's fold-plan paragraph), each ending in a newline; empty when no
# fragment of the <spec>s is in a release yet. Sets groups to its distinct
# <package-dir><TAB><label> pairs, in the plan's order. <label> is empty for
# the whole plan, or the one label fold-notes renders: a spec with a tag
# prefix then reads that group's commits once (group_commits) instead of
# asking the tags about each fragment.
fold_plan() {
  local only=$1
  shift
  check_specs "$@"
  local spec dir prefix index=0 names=' ' head head_label head_date entry meta path commit candidate tag version label date lines=''
  local group_tag='' group_date=''
  local unexpected=()
  local -A in_group=() tag_dates=()
  head=$(git rev-parse HEAD)
  head_label="main@${head:0:12}"
  head_date=$(commit_date HEAD)
  for spec in "$@"; do
    dir=${spec%%=*} prefix=''
    [[ $spec != *=* ]] || prefix=${spec#*=}
    [[ $names != *" ${dir##*/} "* ]] || fail_error "two package specs name a directory ${dir##*/}; the notes files are named by it, so each must differ"
    names+="${dir##*/} "
    git cat-file -e "HEAD:$dir/CHANGELOG.md" 2>/dev/null || fail_error "$dir holds no CHANGELOG.md at HEAD, which every package whose fragments are rendered holds"
    index=$((index + 1))
    if [ -n "$prefix" ] && [ -n "$only" ]; then
      # The whole group at once, and the date of its tag with it: a spec whose
      # label no tag of the prefix carries holds no fragment of the group.
      group_commits "$prefix" "$only" || continue
      group_date=$(commit_date "$group_tag^{commit}")
    fi
    # Every entry directly in the unreleased directory, as HEAD's tree holds
    # them (no -r, so a subdirectory is one tree entry): a regular .yaml file
    # is a fragment, and anything else is refused below.
    while IFS= read -r -d '' entry; do
      meta=${entry%%$'\t'*} path=${entry#*$'\t'}
      if ! [[ $meta == 100644\ blob\ * || $meta == 100755\ blob\ * ]] || [[ $path != *.yaml ]]; then
        unexpected+=("$path")
        continue
      fi
      [[ $path != *[$'\t\n']* ]] || fail_error "fragment $(quote_all "$path") has a tab or a newline in its name; rename it"
      if [ -z "$prefix" ]; then
        label=$head_label date=$head_date
      elif [ -n "$only" ]; then
        commit=$(first_added "$path")
        [ -n "${in_group[$commit]:-}" ] || continue
        label=$only date=$group_date
      else
        commit=$(first_added "$path")
        label='' tag=''
        for candidate in $(git tag --list "$prefix*" --contains "$commit"); do
          version=${candidate#"$prefix"}
          is_plain_version "$version" || continue
          if [ -z "$label" ] || version_gt "$label" "$version"; then
            label=$version tag=$candidate
          fi
        done
        [ -n "$label" ] || continue
        # Every fragment of a group has the same tag, so its date is read once.
        if [ -z "${tag_dates[$tag]:-}" ]; then
          tag_dates[$tag]=$(commit_date "$tag^{commit}")
        fi
        date=${tag_dates[$tag]}
      fi
      lines+="$index"$'\t'"$dir"$'\t'"$label"$'\t'"$date"$'\t'"$path"$'\n'
    done < <(git ls-tree -z HEAD -- "$dir/.changes/unreleased/")
  done
  if [ ${#unexpected[@]} -gt 0 ]; then
    mapfile -d '' -t unexpected < <(printf '%s\0' "${unexpected[@]}" | LC_ALL=C sort -z)
    fail_error "$(quote_all "${unexpected[@]}") at HEAD: each entry in a package's .changes/unreleased/ must be a regular file whose name ends in .yaml, a changie fragment, or no release renders it; move it, rename it or remove it. Move or rename a fragment in one commit, with git mv: a fragment removed in one commit and added again in another is a new entry, which the release after it labels"
  fi
  set_plan "$(printf '%s' "$lines" | LC_ALL=C sort -t $'\t' -k1,1n -k3,3V -k5,5 | cut -f2-)"
}

# set_plan <lines>: set plan to <lines>, plan lines without the last newline,
# each then ending in a newline (empty when <lines> is), and groups to its
# distinct <package-dir><TAB><label> pairs, in the plan's order.
set_plan() {
  plan=$1
  [ -z "$plan" ] || plan+=$'\n'
  groups=$(printf '%s' "$plan" | cut -f1,2 | uniq)
}

# unlisted_files <spec>...: set unlisted to every path HEAD's tree holds under
# the .changes/ of a package no <spec> names, sorted; an empty array when there
# is none. fold-plan warns about them and fold-orphans refuses them (the
# header's paragraph for each). It reads HEAD's tree, as fold_plan does, never
# the working tree, and the package a path belongs to is read from the path
# with a bash regex, so the only pathspec is packages/, which holds no magic.
unlisted_files() {
  local spec path listed=' '
  unlisted=()
  for spec in "$@"; do
    listed+="${spec%%=*} "
  done
  while IFS= read -r -d '' path; do
    [[ $path =~ ^(packages/[^/]+)/\.changes/ ]] || continue
    [[ $listed == *" ${BASH_REMATCH[1]} "* ]] || unlisted+=("$path")
  done < <(git ls-tree -r -z --name-only HEAD -- packages/)
  [ ${#unlisted[@]} -eq 0 ] || mapfile -d '' -t unlisted < <(printf '%s\0' "${unlisted[@]}" | LC_ALL=C sort -z)
}

# unlisted_message <spec>...: what fold-plan's ::warning:: and fold-orphans'
# ::error:: both say about the paths unlisted_files left in unlisted: every one
# of them, the specs the command was given, and what to do with the file.
unlisted_message() {
  printf '%s at HEAD: each is under the .changes/ of a package this command was not given, here %s, and it reads the fragments of the packages it is given alone, so no release renders it and it stays where it is; remove it, or give its package a .changie.yaml and a CHANGELOG.md and add it to .github/changelog-packages' "$(quote_all "${unlisted[@]}")" "$(quote_all "$@")"
}

# group_paths <dir> <label>: the path, the plan's fourth column, on each line
# of that group.
group_paths() {
  awk -F '\t' -v dir="$1" -v label="$2" '$1 == dir && $2 == label { print $4 }' <<<"$plan"
}

# notes_name <package-dir> <label>: the name of the notes file of that group.
notes_name() { printf '%s\n' "${1##*/}_$2.md"; }

# render_notes <package-dir> <out> <fragment>...: run `changie batch 0.0.0` over
# those fragments, as HEAD holds them, in a directory of their own beside a copy
# of <package-dir>/.changie.yaml, and copy the notes it renders to <out> (none
# when <out> is empty). On a failure, changie's output is left in
# changie_output and the status is 1.
render_notes() {
  local package_dir=$1 out=$2 fragment render status=0
  shift 2
  render=$(mktemp -d)
  mkdir -p "$render/.changes/unreleased"
  git show "HEAD:$package_dir/.changie.yaml" > "$render/.changie.yaml" || status=1
  for fragment in "$@"; do
    git show "HEAD:$fragment" > "$render/.changes/unreleased/${fragment##*/}" || status=1
  done
  if [ "$status" = 0 ] && changie_output=$(cd "$render" && changie batch 0.0.0 2>&1 </dev/null); then
    if [ -n "$out" ]; then cp "$render/.changes/0.0.0.md" "$out" || status=1; fi
  else
    status=1
  fi
  rm -rf "$render"
  return "$status"
}

# land_on_plugin_branch <onto-sha> <workflow-file>: land HEAD, exactly one
# commit whose one parent is <onto-sha>, on $plugin_branch with the deploy key
# when main's tip is still <onto-sha>, and otherwise push nothing and start a
# fresh run of <workflow-file> on main (the header's land_on_plugin_branch
# paragraph, under push-built, which ends with it). Never rebases. Returns 0
# when it pushed or started a run, leaving which in $landed (pushed or
# restarted); every other outcome is an ::error:: and exit 1.
land_on_plugin_branch() {
  local onto=$1 workflow=$2 shas head parents tip why
  landed=''
  # HEAD's sha, then its parents, space-separated: exactly one, onto.
  shas=$(git rev-list --parents -n 1 HEAD) || fail_error "reading HEAD failed"
  head=${shas%% *}
  parents=${shas#"$head"}
  parents=${parents# }
  if [ "$parents" != "$onto" ]; then
    fail_error "HEAD $head has the parents '$parents', not exactly the one parent $onto; only one commit made on $onto lands on $plugin_branch"
  fi
  deploy_key_env
  # main, not $plugin_branch: HEAD was made on main's tip, so main's tip is
  # what decides whether this commit is still the one to push.
  git fetch "$remote" main || fail_error "fetching main failed"
  tip=$(git rev-parse FETCH_HEAD) || fail_error "reading main's fetched tip failed"
  if [ "$tip" = "$onto" ]; then
    # Forced, because the plugin branch's tip is the previous build's commit and
    # never an ancestor of this one, so no push of it is a fast-forward.
    if git push --force "$remote" "HEAD:refs/heads/$plugin_branch"; then
      landed=pushed
      return 0
    fi
    git fetch "$remote" main || fail_error "the push to $plugin_branch was rejected, and fetching main again failed"
    tip=$(git rev-parse FETCH_HEAD) || fail_error "reading main's fetched tip failed"
    if [ "$tip" = "$onto" ]; then
      fail_error "the push of $head to $plugin_branch was rejected while main's tip is still $onto, the commit $head was made on, so no newer commit beat it; the push itself failed"
    fi
  fi
  # main moved past onto: what HEAD holds was made on an older main, so a fresh
  # run works from main's tip instead.
  why="main moved from $onto, the commit $head was made on, to $tip"
  fresh_run "$workflow" "$why" "Nothing pushed; a fresh run started" "$why, so $head was not pushed;"
  landed=restarted
}

case "$command" in
  tag)
    version=${2:?version}
    tag=${3:?tag}
    is_plain_version "$version" ||
      fail_error "the release version $(quote_all "$version") is not X.Y.Z, each field a number without leading zeros; refusing it"
    sha=${GITHUB_SHA:-}
    is_commit_sha "$sha" ||
      fail_error "GITHUB_SHA names the commit this run releases; $(quote_all "$sha") is not 40 lower-case hex"
    # The tags are fetched so a tag pushed since the checkout is seen: a
    # re-run, or a second run on this commit, reuses it instead of failing to
    # make it. Nothing is committed and nothing is written: the tag names the
    # commit the run started on, and the tags record every version.
    git fetch --tags origin
    if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
      tagged_commit=$(git rev-parse "$tag^{commit}")
      if [ "$tagged_commit" != "$sha" ]; then
        fail_error "tag $tag already exists on $tagged_commit, not $sha, the commit this run releases"
      fi
      echo "Tag $tag already exists for version $version; reusing it"
    else
      git tag "$tag" "$sha"
    fi
    ;;
  tagged)
    pushed_tag "${2:?tag prefix}"
    ;;
  newest-version)
    prefix=${2:?tag prefix}
    tag=$(newest_tag "$prefix")
    version=${tag#"$prefix"}
    # The newest tag decides, and is checked after: a name no release made
    # prints nothing rather than reaching past it to an older tag.
    if is_plain_version "$version"; then
      printf '%s\n' "$version"
    fi
    ;;
  version)
    shift
    usage='usage: release-push.sh version <tag-prefix> [--first-npm <npm package>] [--expect <X.Y.Z>] [--patch-path <path>]... -- <paths>...'
    if [ $# -lt 1 ] || [[ $1 == -* ]]; then
      echo "::error::release-push.sh version: a tag prefix is needed; $usage" >&2
      exit 2
    fi
    prefix=$1
    shift
    # The prefix names the tags to list and becomes part of the tag printed.
    if ! [[ $prefix =~ ^[A-Za-z0-9._-]+$ ]]; then
      echo "::error::release-push.sh version: tag prefix $(quote_all "$prefix") holds more than letters, digits, '.', '_' and '-'; refusing it" >&2
      exit 2
    fi
    first_npm='' expect=''
    bump_args=()
    while [ $# -gt 0 ]; do
      case $1 in
        --first-npm | --expect | --patch-path)
          if [ $# -lt 2 ]; then
            echo "::error::release-push.sh version: $1 needs a value; $usage" >&2
            exit 2
          fi
          case $1 in
            --first-npm) first_npm=$2 ;;
            --expect)
              if ! is_plain_version "$2"; then
                echo "::error::release-push.sh version: --expect $(quote_all "$2") is not X.Y.Z, each field a number without leading zeros; refusing it" >&2
                exit 2
              fi
              expect=$2
              ;;
            --patch-path) bump_args+=(--patch-path "$2") ;;
          esac
          shift 2
          ;;
        --) shift; break ;;
        *) echo "::error::release-push.sh version: unknown option $(quote_all "$1"); $usage" >&2; exit 2 ;;
      esac
    done
    # The remaining arguments are the paths release-bump.sh reads commits for.
    summary_line='' on_head=false
    tag=$(pushed_tag "$prefix")
    if [ -n "$tag" ]; then
      version=${tag#"$prefix"}
      skip=false on_head=true
      log="Releasing $tag, already pushed for this checkout"
    else
      prev_tag=$(newest_tag "$prefix")
      if [ -n "$prev_tag" ]; then
        prev_version=${prev_tag#"$prefix"}
        range="$prev_tag..HEAD"
      elif [ -n "$first_npm" ]; then
        prev_version=$(npm view "$first_npm" version 2>/dev/null || echo 0.0.0)
        range=HEAD
      else
        echo "::error::release-push.sh version: no $prefix* tag; a release line starts from a tag (push ${prefix}0.0.0 on the commit before its first release)" >&2
        exit 1
      fi
      # release-bump.sh refuses a previous version that is not X.Y.Z (the
      # newest tag's name is whatever its pusher chose); that failure is this
      # command's, with nothing on stdout. Its status passes through, except a 3
      # or 4 (a sed it pipes into exits 4 on an I/O error), which --expect's
      # caller would read as another run's tag: that is a 1.
      next=$("$(dirname "${BASH_SOURCE[0]}")/release-bump.sh" "$prev_version" "$range" "${bump_args[@]}" -- "$@") || {
        status=$?
        case $status in 3 | 4) exit 1 ;; *) exit "$status" ;; esac
      }
      since=${prev_tag:-the beginning}
      case $next in
        empty)
          version='' tag='' skip=true
          log="No commits in the release's paths since $since"
          ;;
        none)
          version='' tag='' skip=true
          log="::notice title=No release published::No version-bumping commits since $since"
          summary_line="**No release published** — No version-bumping commits since $since"
          ;;
        *)
          version=$next tag=$prefix$next skip=false
          log="Bumping ${prev_tag:-v$prev_version} -> $prefix$next"
          ;;
      esac
    fi
    if [ -n "$expect" ]; then
      if $skip; then result='nothing to release'; else result=$version; fi
      if [ "$result" != "$expect" ]; then
        # The tags moved since the build job read them: another run took the
        # tag first. The caller acts on which of the two statuses this is.
        lead="release-push.sh version: the build job handed v$expect"
        if $skip; then
          echo "::notice title=Nothing left to release::$lead; the tags now give nothing to release: another run took the tag first, and nothing of this checkout is left to release" >&2
          exit 3
        elif $on_head; then
          echo "::notice title=Released by another run::$lead; another run took the tag first and released this checkout as $tag" >&2
          exit 3
        fi
        echo "::notice title=Version taken by another run::$lead; the tags now give $version: another run took the tag first, and this checkout holds commits no release holds yet, so a new run must compute the version again and build them" >&2
        exit 4
      fi
    fi
    # release-bump.sh's answer is checked once more right before it goes to $GITHUB_OUTPUT.
    if [ -n "$version" ] && ! is_plain_version "$version"; then
      echo "::error::release-push.sh version: the version $(quote_all "$version") is not X.Y.Z; refusing it" >&2
      exit 1
    fi
    echo "$log" >&2
    if [ -n "$summary_line" ] && [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
      printf '%s\n' "$summary_line" >> "$GITHUB_STEP_SUMMARY"
    fi
    printf 'version=%s\ntag=%s\nskip=%s\n' "$version" "$tag" "$skip"
    ;;
  restart)
    usage='usage: release-push.sh restart <workflow-file> <lost-races>'
    if [ $# -ne 3 ]; then
      echo "::error::release-push.sh restart: $usage" >&2
      exit 2
    fi
    workflow=$2 lost=$3
    if [ -z "${GITHUB_REPOSITORY:-}" ] || [ -z "${GITHUB_REF_NAME:-}" ] ||
      ! [[ ${GITHUB_RUN_NUMBER:-} =~ ^[1-9][0-9]*$ ]]; then
      echo "::error::release-push.sh restart: needs \$GITHUB_REPOSITORY, \$GITHUB_REF_NAME and \$GITHUB_RUN_NUMBER as GitHub Actions sets them" >&2
      exit 2
    fi
    branch=$GITHUB_REF_NAME
    by_hand="gh workflow run $workflow --repo $GITHUB_REPOSITORY --ref $branch"
    # The count goes into arithmetic below, where bash would run a[$(…)].
    if ! [[ $lost =~ ^(0|[1-9][0-9]?)$ ]]; then
      echo "::error title=No release run started::release-push.sh restart: lost_races $(quote_all "$lost") is not a count from 0 to 99; start a run by hand: $by_hand" >&2
      exit 2
    fi
    head=$(git rev-parse HEAD)
    # Each in a command substitution, so a gh or jq failure fails the command.
    runs=$(gh run list --repo "$GITHUB_REPOSITORY" --workflow "$workflow" --branch "$branch" \
      --limit 50 --json databaseId,number,headSha,status)
    later=$(jq -r --argjson self "$GITHUB_RUN_NUMBER" \
      '.[] | select(.status != "completed" and .number > $self) | "\(.databaseId) \(.headSha)"' <<< "$runs")
    # The branch is fetched after the list, so every run the list names has its commit here; a
    # commit still missing counts as not holding HEAD, and a run is started.
    git fetch -q origin "refs/heads/$branch"
    while read -r id sha; do
      { [[ $id =~ ^[0-9]+$ ]] && is_commit_sha "$sha"; } || continue
      if git merge-base --is-ancestor "$head" "$sha" 2>/dev/null; then
        echo "::notice title=No release run started::another run took this run's version first; run ${GITHUB_SERVER_URL:-https://github.com}/$GITHUB_REPOSITORY/actions/runs/$id, started later and not finished, is on $branch at $sha, which holds this commit: it releases this commit, or fails where a person sees it" >&2
        exit 0
      fi
    done <<< "$later"
    if [ "$lost" -ge 3 ]; then
      echo "::error title=No release run started::release-push.sh restart: this run and the $lost before it each lost their version to another run; start a run by hand once releases stop landing: $by_hand" >&2
      exit 1
    fi
    next=$((lost + 1))
    # gh prints the new run's URL: gh 2.102.0 on ubuntu-24.04 did on 2026-10-25.
    started=$(gh workflow run "$workflow" --repo "$GITHUB_REPOSITORY" --ref "$branch" -f "lost_races=$next")
    echo "::notice title=Release run started::another run took this run's version first, so a new run on $branch, ${started:-the newest run of $workflow}, releases this commit (lost_races $next)" >&2
    ;;
  release)
    shift
    usage='usage: release-push.sh release [--notes <text>] [--upload-to-published] <tag> <title> <asset>...'
    notes=(--generate-notes)
    upload_to_published=false
    while [ $# -gt 0 ]; do
      case $1 in
        --notes)
          if [ $# -lt 2 ]; then
            echo "::error::release-push.sh release: --notes needs a text; $usage"
            exit 2
          fi
          notes=(--notes "$2")
          shift 2
          ;;
        --upload-to-published) upload_to_published=true; shift ;;
        -*) echo "::error::release-push.sh release: unknown option $1; $usage"; exit 2 ;;
        *) break ;;
      esac
    done
    if [ $# -lt 3 ]; then
      echo "::error::release-push.sh release: a tag, a title and at least one asset are needed; $usage"
      exit 2
    fi
    tag=$1 title=$2
    shift 2
    repo=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must name the repository as owner/repo}
    if ! is_draft=$(gh release view "$tag" --repo "$repo" --json isDraft -q .isDraft 2>/dev/null); then
      gh release create "$tag" "$@" --repo "$repo" --title "$title" "${notes[@]}" --verify-tag
      exit 0
    fi
    case $is_draft in
      true)
        gh release upload "$tag" "$@" --repo "$repo" --clobber || {
          echo "::error::Uploading this run's assets to the draft release $tag failed; the draft is left unpublished."
          exit 1
        }
        gh release edit "$tag" --repo "$repo" --draft=false || {
          echo "::error::Publishing the draft release $tag failed; it holds this run's assets and is still a draft."
          exit 1
        }
        echo "Published the draft release $tag with this run's assets"
        ;;
      false)
        if [ "$upload_to_published" = true ]; then
          gh release upload "$tag" "$@" --repo "$repo" --clobber || {
            echo "::error::Uploading this run's assets to the published release $tag failed."
            exit 1
          }
          echo "Release $tag already exists; uploaded this run's assets over its own"
        else
          echo "Release $tag already exists; reusing it"
        fi
        ;;
      *)
        printf -v shown '%q' "$is_draft"
        echo "::error::gh release view $tag printed $shown for isDraft, not true or false"
        exit 1
        ;;
    esac
    ;;
  release-section)
    shift
    usage='usage: release-push.sh release-section <tag> <heading> <content-file>'
    if [ $# -ne 3 ]; then
      echo "::error::release-push.sh release-section: a tag, a heading and a content file are needed; $usage" >&2
      exit 2
    fi
    tag=$1 heading=$2 content=$3
    repo=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must name the repository as owner/repo}
    # The heading is how a later run finds the section it must replace, so it is
    # refused unless it is a level-two heading line: under any other heading the
    # awk below would find nothing and a re-run would add a second copy.
    if [[ $heading != '## '[^[:space:]]* ]] || [[ $heading == *[[:space:]] ]]; then
      echo "::error::release-push.sh release-section: the heading $(quote_all "$heading") is not a level-two heading line ('## ' and a title); refusing it" >&2
      exit 2
    fi
    section_dir=$(mktemp -d)
    on_exit_remove "$section_dir"
    if [ "$content" = - ]; then
      shown='standard input'
      content=$section_dir/content.md
      cat > "$content"
    else
      shown=$(quote_all "$content")
    fi
    # A heading over nothing is no section, and an empty file is a caller whose
    # content was never rendered: either way the body is left as it is.
    [ -s "$content" ] ||
      fail_error "nothing to write into the $heading section of release $tag: $shown holds no content"
    body=$section_dir/body.md new=$section_dir/new-body.md
    gh release view "$tag" --repo "$repo" --json body -q .body > "$body"
    # Every other line of the body is kept in its order; the section runs to the
    # next heading of level 1 or 2. The heading goes to awk through the
    # environment, which passes it as it is, where -v would read a backslash in
    # it as an escape and so match no line of the body.
    rest=$(section_heading=$heading awk '
      { line = $0; sub(/[ \t\r]+$/, "", line) }
      line == ENVIRON["section_heading"] { skip = 1; next }
      skip && /^##? / { skip = 0 }
      !skip
    ' "$body")
    if [ -n "$rest" ]; then
      printf '%s\n\n' "$rest" > "$new"
    else
      : > "$new"
    fi
    { printf '%s\n\n' "$heading"; cat "$content"; } >> "$new"
    # $(<…) drops trailing newlines, so a body differing only in those is unchanged.
    if [ "$(<"$body")" = "$(<"$new")" ]; then
      echo "Release $tag already holds this $heading section"
      exit 0
    fi
    gh release edit "$tag" --repo "$repo" --notes-file "$new"
    echo "Wrote the $heading section into the body of release $tag"
    ;;
  push-tag)
    tag=${2:?tag}
    deploy_key_env
    git push "$remote" "refs/tags/$tag"
    ;;
  check-built)
    [ $# -eq 2 ] || fail_error "usage: release-push.sh check-built <tar>"
    check_built "$2"
    ;;
  extract-built)
    [ $# -eq 2 ] || fail_error "usage: release-push.sh extract-built <tar>"
    extract_built "$2"
    ;;
  push-built)
    [ $# -eq 3 ] || fail_error "usage: release-push.sh push-built <base-sha> <tar>"
    base=$2 tar_file=$3
    if ! is_commit_sha "$base"; then
      fail_error "the base commit $(quote_all "$base") is not 40 lower-case hex; refusing it"
    fi
    # Everything below is read from the repository's root: the work tree's
    # status, the build input list, the plugin branch's build and the two
    # directories. The job runs this from the checkout's root, but a run from a
    # subdirectory must land the same commit, where a pathspec read from there
    # would find nothing. extract_built moves to the root as well, and
    # branch_build_base and build_inputs_changed run before it. <tar> is the
    # caller's path, so it is made absolute before the move, as extract_built
    # does with its own.
    case $tar_file in /*) ;; *) tar_file=$PWD/$tar_file ;; esac
    root=$(git rev-parse --show-toplevel) || fail_error "finding the repository's root failed"
    cd "$root"
    head=$(git rev-parse HEAD) || fail_error "reading HEAD failed"
    if [ "$head" != "$base" ]; then
      fail_error "HEAD is $head, not $base, the commit the build was made from"
    fi
    dirty=$(git status --porcelain) || fail_error "reading the work tree's status failed"
    if [ -n "$dirty" ]; then
      fail_error "the work tree is not clean (${dirty//$'\n'/; }); the commit holds the build and nothing else"
    fi
    # The tar is checked before anything reaches GitHub, so a doctored build is
    # refused however main stands (extract_built checks it again below).
    check_built "$tar_file"
    workflow=claude-envoy-rebuild.yaml
    deploy_key_env
    git fetch "$remote" main || fail_error "fetching main failed"
    tip=$(git rev-parse FETCH_HEAD) || fail_error "reading main's fetched tip failed"
    if ! is_ancestor "$base" "$tip"; then
      why="main's tip $tip does not descend from $base, the commit the build was made from, so main was rewritten"
      fresh_run "$workflow" "$why" "Nothing pushed; a fresh run started" "$why; nothing was pushed, and"
      exit 0
    fi
    # The branch the build lands on, fetched beside main. The remote need not
    # hold it - before the first rebuild lands there is no branch and no
    # recorded build - and `git fetch` of a ref the remote lacks is a failure,
    # so the branch is looked up first and fetched only when it is there.
    branch_tip=''
    branch_ref=$(git ls-remote --heads "$remote" "refs/heads/$plugin_branch") ||
      fail_error "listing $plugin_branch on the remote failed"
    if [ -n "$branch_ref" ]; then
      git fetch "$remote" "$plugin_branch" || fail_error "fetching $plugin_branch failed"
      branch_tip=$(git rev-parse FETCH_HEAD) || fail_error "reading $plugin_branch's fetched tip failed"
    fi
    built_base=$(branch_build_base "$branch_tip") || exit 1
    if [ -n "$built_base" ] && ! is_ancestor "$built_base" "$base"; then
      echo "::notice title=Nothing pushed; $plugin_branch holds a newer build::$plugin_branch already holds a build of $built_base, newer than $base, the commit this build was made from, so nothing was pushed and no run was started" >&2
      exit 0
    fi
    inputs_why=''
    if [ "$tip" != "$base" ]; then
      build_inputs_changed "$base" "$tip"
    fi
    extract_built "$tar_file"
    commit_build_on "$tip" "$base" "$branch_tip"
    if [ -z "$commit" ]; then
      echo "::notice title=Nothing to push::$plugin_branch's packages/claude-envoy/dist and packages/claude-envoy/legion at $branch_tip already match this build" >&2
      if [ -n "$inputs_why" ]; then
        fresh_run "$workflow" "$inputs_why" "A fresh run started" "$inputs_why, so"
      fi
      exit 0
    fi
    if [ "$tip" != "$base" ]; then
      echo "::notice title=Landing on $plugin_branch::built from $base, landing on $plugin_branch as $commit, made on $tip, main's tip, which changes only packages/claude-envoy/dist and packages/claude-envoy/legion: main moved since the build started" >&2
    fi
    # land_on_plugin_branch fetches main again and pushes only when main's tip
    # is still the tip the commit was made on; when main moved again since the
    # fetch above, it starts the one fresh run itself.
    land_on_plugin_branch "$tip" "$workflow"
    if [ "$landed" = pushed ] && [ -n "$inputs_why" ]; then
      why="the build already landed on $plugin_branch as $commit, but $inputs_why, so the plugin branch's build is one run behind its inputs"
      fresh_run "$workflow" "$why" "A fresh run started" "the build landed on $plugin_branch as $commit, but $inputs_why, so"
    fi
    ;;
  handed)
    if [ $# -lt 5 ]; then
      echo "::error::release-push.sh handed: usage: release-push.sh handed <tag-prefix> <version> <dir> <file>..." >&2
      exit 1
    fi
    prefix=$2
    version=$3
    dir=${4%/}
    shift 4
    if ! is_plain_version "$version"; then
      echo "::error::release-push.sh handed: version $(quote_all "$version") is not X.Y.Z, each field a number without leading zeros; refusing it" >&2
      exit 1
    fi
    check_hand_over "$dir" "the release does not ship" "$@"
    printf '%s\n' "$prefix$version"
    ;;
  fold-plan)
    shift
    fold_plan '' "$@"
    unlisted_files "$@"
    [ ${#unlisted[@]} -eq 0 ] || echo "::warning::release-push.sh $command: $(unlisted_message "$@")" >&2
    printf '%s' "$plan"
    ;;
  fold-orphans)
    shift
    check_specs "$@"
    unlisted_files "$@"
    [ ${#unlisted[@]} -eq 0 ] || fail_error "$(unlisted_message "$@")"
    ;;
  fold-notes)
    shift
    usage='usage: release-push.sh fold-notes [--label <label>] <notes-dir> <spec>...'
    only_label=''
    while [ $# -gt 0 ]; do
      case $1 in
        --label)
          [ $# -ge 2 ] || fail_error "--label needs a label; $usage"
          only_label=$2
          shift 2
          ;;
        --) shift; break ;;
        -*) fail_error "unknown option $(quote_all "$1"); $usage" ;;
        *) break ;;
      esac
    done
    [ $# -ge 2 ] || fail_error "$usage"
    notes_dir=${1%/}
    shift
    if [ -e "$notes_dir" ] || [ -L "$notes_dir" ]; then
      if [ ! -d "$notes_dir" ] || [ -L "$notes_dir" ] || [ -n "$(ls -A "$notes_dir")" ]; then
        fail_error "$(quote_all "$notes_dir") exists and is not an empty directory; the notes go into a directory of their own"
      fi
    fi
    # The label reaches fold_plan, which renders one release's own group: the
    # plan holds a group for every release the fragments have ever shipped in,
    # and a spec with no tag prefix labels its fragments main@<commit> all the
    # same, so the plan is narrowed here too.
    fold_plan "$only_label" "$@"
    if [ -n "$only_label" ]; then
      set_plan "$(printf '%s' "$plan" | awk -F '\t' -v label="$only_label" '$2 == label')"
    fi
    mkdir -p "$notes_dir"
    if [ -z "$plan" ]; then
      echo "Nothing to render"
      exit 0
    fi
    while IFS=$'\t' read -r dir label; do
      git cat-file -e "HEAD:$dir/.changie.yaml" 2>/dev/null || fail_error "$dir holds no .changie.yaml at HEAD to render its fragments with"
      mapfile -t fragments < <(group_paths "$dir" "$label")
      notes_file="$notes_dir/$(notes_name "$dir" "$label")"
      if ! render_notes "$dir" "$notes_file" "${fragments[@]}"; then
        # changie does not name the fragment of every refusal (an unknown kind):
        # each fragment is rendered alone to find the ones it refuses.
        output=$changie_output
        refused=()
        for fragment in "${fragments[@]}"; do
          render_notes "$dir" '' "$fragment" || refused+=("$fragment")
        done
        if [ ${#refused[@]} -gt 0 ]; then
          output="refused alone: $(quote_all "${refused[@]}"); changie said: $output"
        fi
        fail_error "changie could not render $dir's notes for $label; ${output//$'\n'/ }"
      fi
      echo "Wrote $notes_file from ${#fragments[@]} fragments"
    done <<<"$groups"
    ;;
  no-credential)
    status=0
    echo "git config entries matching credential|insteadof|sshcommand|extraheader (names only):"
    # --get-regexp exits 1 on no match, in or out of a repository (outside one
    # it still reads the global and system files); any other failure means the
    # configuration could not be read, which is not the clean case.
    rc=0
    matches=$(git config --show-origin --name-only --get-regexp 'credential|insteadof|sshcommand|extraheader') || rc=$?
    case $rc in
      0)
        printf '  %s\n' "${matches//$'\n'/$'\n  '}"
        echo "::error::git holds credential or SSH configuration"
        status=1
        ;;
      1) echo "  none" ;;
      *)
        echo "::error::git config --get-regexp exited $rc, so the git configuration could not be read"
        status=1
        ;;
    esac
    # shellcheck disable=SC2088 # the label of the listing, not a path
    echo "~/.ssh:"
    # The trailing slash follows a ~/.ssh that is a symlink to a directory; anything else at that
    # path (a file, a dangling link) is no runner's own and is refused like a key.
    if [ -d ~/.ssh/ ]; then
      ls -la ~/.ssh/
      if [ -n "$(find ~/.ssh/ -mindepth 1 ! -name known_hosts)" ]; then
        echo "::error::~/.ssh holds entries other than known_hosts"
        status=1
      fi
    elif [ -e ~/.ssh ] || [ -L ~/.ssh ]; then
      ls -la ~/.ssh
      echo "::error::~/.ssh is not a directory"
      status=1
    else
      echo "  absent"
    fi
    for var in GIT_SSH_COMMAND SSH_AUTH_SOCK DEPLOY_KEY ACTIONS_ID_TOKEN_REQUEST_URL ACTIONS_ID_TOKEN_REQUEST_TOKEN; do
      if [ -n "${!var+x}" ]; then
        echo "$var: set"
        echo "::error::$var is set"
        status=1
      else
        echo "$var: unset"
      fi
    done
    if [ "$status" = 0 ]; then echo "no credential reachable"; fi
    exit "$status"
    ;;
  *)
    echo "::error::unknown release-push command: $command"
    exit 1
    ;;
esac

