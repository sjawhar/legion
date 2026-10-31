#!/usr/bin/env bash
# Release source-state publisher, shared by the release workflows and by
# claude-envoy-rebuild.yaml, which is no release but pushes to main with the
# same deploy key.
#
# The workflows split each release into a build job, which runs the
# dependencies' code and holds no credential, and a publish job, which holds
# the credentials and runs no dependency code (the rebuild workflow's push job
# is one). `version`, `check-built`, `extract-built`, `no-credential` and
# `fold-notes` are the build job's; `tag`, `release`, `restart`, `push-main`,
# `push-tag`, `push-fold` and `push-built` the publish job's; `tagged`,
# `handed` and `fold-plan` serve both. changelog-fold.yaml splits each fold of
# the packages' changelog fragments the same way.
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
# tag:       commit the version bump and create the release tag on that
#            un-rebased commit, so the tag always describes exactly the source
#            the artifacts were built and published from; pushes nothing
#            (`push-tag` pushes). A pre-existing tag is reused only when it
#            carries the same package version; any other collision fails
#            loudly.
# tagged:    print the release tag an earlier attempt pushed for this checkout,
#            or nothing: the newest <tag-prefix> tag on HEAD itself, or, given
#            <package-dir>, on a child of HEAD that changes only
#            <package-dir>/package.json, the bump commit `tag` makes (the tag
#            sits on HEAD itself when the manifest already held the version and
#            `tag` had nothing to commit). Without <package-dir> only a tag on
#            HEAD counts, as for a job that tags the commit it checked out. A
#            re-run of a release job checks out the same commit, with every tag
#            its first attempt pushed; `version` reads this to release that tag
#            again, where computing the next version from the tag would find an
#            empty range and skip. The version is the tag's name, which anyone
#            who can push a tag chooses, so that tag must be <tag-prefix>X.Y.Z,
#            each field a number without leading zeros: any other name fails
#            with an ::error:: on stderr naming it.
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
# push-main: push the version-bump commit to main, fetching main and rebasing
#            onto it between attempts, five attempts in all. It fetches and
#            pushes $GITHUB_REPOSITORY on GitHub with the deploy key in
#            $DEPLOY_KEY, as push-tag does, from a detached HEAD as well as a
#            branch, and it pushes exactly the one bump commit. Before every
#            push, the first and each one after a rebase, it fetches main and
#            checks HEAD against the fetched tip and against HEAD's own parent:
#            HEAD has one parent; HEAD is the one commit main does not have;
#            HEAD's tree differs from its parent's — main's fetched tip, or an
#            older main commit it is then rebased onto and checked against
#            again — in <package-dir>/package.json alone; and that manifest is
#            its parent's copy with .version set to <version> and nothing
#            else, as JSON (sorted-JSON equality: key order, a BOM, duplicate
#            keys and the blob's mode are not compared; the test `tagged`
#            applies to a bump commit, plus its content). The content checks
#            run before any push, rebase or superseded-version exit, so a
#            doctored commit on an older main commit is refused, not rebased
#            or dropped. A HEAD whose one parent is an older main commit, and
#            passes them, is rebased onto the tip, which replays the one bump
#            over what landed since the checkout, and checked again there.
#            Anything else — a second commit, a merge commit, a parentless
#            commit, a second path, a manifest changed in more than its
#            version — is refused, origin untouched: the only commit a run
#            dispatched from a branch other than main can land is the exact
#            version bump, which is what a run from main lands. A HEAD main
#            already holds (the manifest already held the version, so `tag`
#            committed nothing) has nothing to push and exits 0. This runs
#            last, after the tag and GitHub release exist: a lost push only
#            delays the metadata bump on main, which the tag-anchored version
#            computation heals on the next release. Raced-in commits stay
#            outside the tag and are picked up by the next version calculation.
#
#            The race that needs help is two releases of the *same* package.
#            A run checks out the merge commit that triggered it, and when the
#            previous run's bump commit landed after that merge (bump commits
#            are "[skip ci]" and never trigger a run of their own), both bumps
#            rewrite the one "version" line of the same package.json from the
#            same base and the rebase conflicts. One rule resolves it: when the
#            only conflicted path is the released package's package.json, take
#            main's copy of that file, set its .version to the version being
#            released, continue the rebase, and push again. Any other
#            conflicted path — or the manifest deleted on main — is a real
#            divergence: the rebase is aborted and the step fails naming the
#            paths. Exhausting the attempts fails the step too. Every failure
#            here — a rejected push, an unreachable origin, a rebase that stops
#            for any other reason — says, in the log and in the job summary,
#            that the publish, tag and GitHub release already succeeded and
#            only this push to main is outstanding.
#
#            Before it rebases — after a rejected push, or when HEAD's parent is
#            an older main commit — push-main reads main's manifest. When that
#            manifest already holds the version being released or a later one,
#            there is nothing to push: a re-run whose first attempt landed this
#            bump, or a re-run of an older run after a later release of the
#            package landed. It says so in a notice and exits 0, dropping the
#            bump commit whole, which is right only because the content checks
#            above, run before this exit, found that commit to change nothing
#            but the manifest's version. A push that fast-forwards main is not
#            checked against main's version: it can lower main's manifest only
#            when main held a version above the newest tag, which only a hand
#            edit makes, and the tags decide every next version anyway.
# push-tag:  push the local tag <tag> to $GITHUB_REPOSITORY on GitHub over SSH
#            with the deploy key in $DEPLOY_KEY, for a job whose checkout keeps
#            no credential. The key and GitHub's host keys, pinned below, go
#            into a directory of its own that only the runner's user can read,
#            removed when the script exits, pushed or not (deploy_key_env,
#            which push-main, push-fold and push-built share). ssh reads no
#            config file, offers only that key, never prompts, and refuses any
#            host key but the pinned ones; GitHub publishes them at `gh api
#            meta --jq .ssh_keys`, which release-push.test.sh compares with
#            these when it can reach GitHub.
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
# push-built: land a build of the Claude Envoy plugin on main, for
#            claude-envoy-rebuild.yaml's push job. <tar> is what its build
#            job built from <base-sha>, main's tip when that job started: the
#            package's dist/ and legion/. Every repository path push-built
#            reads is the root's, so it moves to the repository's root (`git
#            rev-parse --show-toplevel`) first, with <tar> made absolute
#            before the move, since that path is the caller's: a run from a
#            subdirectory of the checkout lands the same commit. HEAD must be
#            <base-sha>, 40 lower-case hex, and the work tree clean, which is
#            read from the root too. The tar is then checked as check-built
#            checks it, before anything reaches GitHub, so a tar that fails a
#            check is refused however main stands. Each of these refusals is
#            an ::error:: and exit 1 that pushes nothing and starts no run.
#            push-built then fetches main once with the deploy
#            key (deploy_key_env, as push-tag does). The push job waits for a
#            runner after the build job, often longer than main goes between
#            merges, so main has usually moved past the base by then. The
#            build lands on main's tip all the same, unless one of two rules
#            stops it. When main's tip does not descend from the base, main
#            was rewritten: nothing is pushed, a fresh run of the workflow on
#            main (`gh workflow run`, with $GH_TOKEN) builds the new tip, and
#            a ::notice names both commits. When main already holds a newer
#            build, nothing is pushed, no run is started, and a ::notice says
#            so. Main's build is the newest commit on the tip's first-parent
#            line that touches the two directories. When github-actions[bot]
#            authored it with exactly the subject below, and the 12 hex in that
#            subject name a commit that is its ancestor, it is a build of that
#            commit. Otherwise (a pull request's own committed build, or a
#            subject a person wrote) it is a build of at most the commit
#            itself. This build is the newer one when that commit is the base
#            or an ancestor of it, and when main holds no such commit at all.
#
#            The build is then put into the checkout as extract-built puts it,
#            and the two directories are written over the tip's tree in a
#            temporary index. The tip is never checked out: it may carry
#            another release-push.sh, and a checkout would rewrite this script
#            while bash runs it. A build the tip's files already match stages
#            nothing: a ::notice says so and nothing is pushed. Otherwise
#            github-actions[bot], as `tag` commits, commits the tree on the tip
#            as `chore(claude-envoy): rebuild dist/ and legion/ from <the
#            base's first 12 hex> [skip ci]`. The subject names the base, the
#            commit the build was made from, which the newer rule above reads;
#            GitHub's skip keyword means the commit starts no CI or release
#            run (the build job ran the private-name check over the bundle
#            already). When main had moved, a ::notice titled "Landing on
#            main's tip" names the base, the tip and the commit, which changes
#            only the two directories. The commit lands by land_on_main
#            (below, under push-fold) on the tip push-built fetched.
#
#            A build of the base landed on a newer tip leaves main's build one
#            run behind its inputs when a build input changed between the two.
#            The inputs are the git pathspecs in
#            packages/claude-envoy/scripts/build-inputs.txt, one per line (an
#            empty line or one starting # is ignored), read from the base with
#            git and compared between the base and the tip with `git diff
#            --no-renames`, so a renamed file counts by its old path and its
#            new one alike. A base without the list, or with no pathspec line
#            in it, counts as changed, with a ::warning naming the file. When
#            an input changed, a fresh run of the workflow on main is started
#            once the build has landed, or when the tip's files already match
#            the build, and a ::notice names the first changed paths and the
#            run gh printed. A failed fresh run is an ::error:: and exit 1 that
#            names the command a person runs; after a landing, it says the
#            build already landed.
#
#            Main can move again between push-built's fetch and the push.
#            land_on_main fetches main again and pushes only when main's tip is
#            still the tip push-built built on, never forced, so nothing lands
#            on a tip push-built did not check. Otherwise land_on_main starts
#            the one fresh run itself, and push-built starts no second one,
#            whether or not an input changed. There is no retry loop: the fresh
#            run is the retry.
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
# fold-plan: print which changelog fragments a fold of main writes into which
#            CHANGELOG.md, one tab-separated line per fragment:
#            <package-dir>, <label>, <date> (YYYY-MM-DD, UTC), <path>, sorted by
#            package in the order of the specs, then by label as versions
#            (`sort -V`), then by path; nothing printed means nothing to fold.
#            Each <spec> is <package-dir>[=<tag-prefix>], and the package must
#            hold a CHANGELOG.md at HEAD. The fragments are the entries directly
#            in <package-dir>/.changes/unreleased/ that HEAD holds (git's tree,
#            never the working tree). Each must be a regular file (mode 100644
#            or 100755) whose name ends in .yaml: any other entry there, such as
#            a subdirectory, a symlink, a submodule or a .yml file, would never
#            be folded, so fold-plan fails with one ::error:: that names every
#            such entry of every spec, sorted, and tells the author to move or
#            rename a fragment in one commit with `git mv`, and prints no plan.
#            A file under the .changes/ of a package no <spec> names is refused
#            the same way, with its own ::error:: naming every such path HEAD's
#            tree holds, sorted, and the specs it was given: the fold folds the
#            fragments of the packages it is given and of no others, so the
#            file would stay on main unfolded. The error says to remove it, or
#            to give its package a .changie.yaml and a CHANGELOG.md and add it
#            to CHANGELOG_PACKAGES in .github/workflows/changelog-fold.yaml.
#            It is the fold-plan command's own check, made after every refusal
#            above and before the plan is printed; fold-notes and push-fold do
#            not make it, since changelog-fold.yaml's build job runs fold-plan
#            first and its publish job needs that job.
#            With a tag prefix (a package with releases of its own), a
#            fragment's label is X.Y.Z of the lowest <tag-prefix>X.Y.Z tag
#            that contains the commit which first added the entry, following
#            renames: the release that first shipped the entry, whether the tag
#            sits on the bump commit `tag` makes over the built commit (the npm
#            plugins) or on the built commit itself (the listener); other names
#            under the prefix are not releases and are skipped. A commit that
#            deletes a file and adds one git pairs with it as a rename, as
#            `git mv` does, renames the entry; a fragment deleted in one commit
#            and added again in another is a new entry, and a copy is never
#            followed. A fragment no such tag contains waits for a later run,
#            unprinted.
#            The date is the tag's commit's committer date. Without a prefix (a
#            package with no release of its own), every fragment is folded under
#            main@<HEAD's first 12 hex digits>, dated by HEAD's committer date.
#            A fragment's path is one file to git, never a pattern: a name
#            holding a glob character gets the commit that added that file. It
#            reads only git, so it computes the same plan from the same
#            checkout and tags. The build job and the publish job check out the
#            same commit, but each fetches every tag, so a release tag pushed
#            between the jobs can give the publish job's plan a version group
#            the build job's lacks (push-fold defers it).
# fold-notes: the build job's half of a fold: render the plan's fragments with
#            changie, which is dependency code, into <notes-dir>, which must be
#            absent or empty and is created. Each (package, label) of the plan
#            gets a directory of its own holding a copy of the package's
#            .changie.yaml and only that group's fragments as HEAD holds them,
#            where `changie batch 0.0.0` renders them, and the rendered notes
#            become <notes-dir>/<package basename>_<label>.md. An empty plan
#            writes nothing. A fragment changie refuses fails the command with
#            an ::error:: naming the package, the label, each fragment changie
#            refuses on its own and changie's output.
# push-fold: the publish job's half: write the build job's notes into the
#            CHANGELOG.md files and land the result on main, with git and gh
#            and the base text tools only (coreutils, find, awk, sed and grep).
#            HEAD must be <base-sha>, the commit the build job built, with a
#            clean working tree and index; $DEPLOY_KEY and $GITHUB_REPOSITORY
#            must be set before anything is written. The plan is computed again
#            from git, never taken from the build job; an empty one prints
#            "Nothing to fold" and exits 0. A group labelled X.Y.Z whose notes
#            file does not exist at all is deferred:
#            its release tag was pushed after the build job computed its plan,
#            so the build rendered no notes for it. Its fragments stay on main,
#            a ::notice titled "Fold deferred" names its package and label, and
#            a later run folds it (the release's own completion starts one, and
#            the daily run is the backstop); the other groups fold as below.
#            When every group is deferred, it says so, pushes nothing, starts
#            no run and exits 0. A main@ group is never deferred: its label
#            depends only on HEAD, which is the base the build job planned
#            from, so its notes are missing only when the build is faulty, and
#            that is a refusal. <notes-dir> must hold exactly the files
#            <package basename>_<label>.md of the groups not deferred, by
#            `handed`'s rule (regular files, nothing else at any depth; a
#            version group's notes path that exists as anything but a regular
#            file is refused, not deferred), so the build job chooses the
#            notes' bytes but not which packages and labels they go to. Each
#            must hold something besides blank lines, no NUL byte and no line
#            starting "## " (the heading is push-fold's to write), and no
#            package's CHANGELOG.md may already hold the heading
#            "## [<label>]" of its group. Every refusal is an ::error:: naming
#            the file, package or label, before anything is written. Each group
#            then becomes a block, applied in ascending label order:
#            "## [<label>] - <date>", a blank line, the notes with leading and
#            trailing blank lines trimmed, a blank line. A version's block goes
#            above the first "## " line that is not the release heading
#            "## [X.Y.Z]" of a higher version, so the newest release is on top
#            and an older one goes below a newer one present; a main@ block
#            goes above the first "## " line; with no such line the block is
#            appended after a blank line. Every existing line is kept, byte for
#            byte and in order. The fragments of the groups folded are removed,
#            and the one commit, by github-actions[bot] as `tag`'s are, has the
#            fixed subject "chore: fold changelog fragments [skip ci]" and no
#            body: "[skip ci]" starts no workflow run from it, and
#            release-bump.sh reads commit subjects and bodies, where a fixed
#            chore subject bumps no release. The commit lands by land_on_main
#            (below), which never rebases: a fold is of main's tip, so when main
#            moved since the build, a new run folds the new tip, fragments and
#            all, and is the retry.
#
#            push-fold and push-built each end with land_on_main <onto-sha>
#            <workflow-file>, so each exits 0 whenever land_on_main returns
#            (push-built unless the fresh run it then starts for a changed
#            build input fails). push-fold passes its base, the commit its
#            build job built; push-built passes main's tip as its own fetch saw
#            it, which is the base when main has not moved. land_on_main never
#            forces and never rebases. It refuses a HEAD that is not exactly
#            one commit whose one parent is <onto-sha>, which refuses any
#            <onto-sha> but that parent's full sha (each caller has refused a
#            base that is not 40 lower-case hex already), then fetches main
#            again with the deploy key (deploy_key_env, as push-tag does). When
#            main's tip is <onto-sha> it pushes HEAD there. When main's tip is
#            another commit, before the push or after a rejected push, it
#            pushes nothing: it starts a fresh run of <workflow-file> on main
#            (`gh workflow run`, with $GH_TOKEN), which builds main's tip, and
#            a ::notice names both commits. In both cases it returns 0, and
#            leaves which it did in $landed (pushed or restarted), so
#            push-built starts no second run after land_on_main started one. A
#            failed fetch, a push rejected while main's tip is still
#            <onto-sha>, or a failed `gh workflow run` is an ::error:: and exit
#            1.
set -euo pipefail

command=${1:?usage: release-push.sh version <tag-prefix> [--package-dir <dir>] [--first-npm <npm package>] [--expect <X.Y.Z>] [--patch-path <path>]... -- <paths>... | release-push.sh restart <workflow-file> <lost-races> | release-push.sh tag <package-dir> <commit-message> <tag> | release-push.sh tagged <tag-prefix> [<package-dir>] | release-push.sh release [--notes <text>] [--upload-to-published] <tag> <title> <asset>... | release-push.sh push-main <package-dir> <version> | release-push.sh push-tag <tag> | release-push.sh check-built <tar> | release-push.sh extract-built <tar> | release-push.sh push-built <base-sha> <tar> | release-push.sh handed <tag-prefix> <version> <dir> <file>... | release-push.sh fold-plan <spec>... | release-push.sh fold-notes <notes-dir> <spec>... | release-push.sh push-fold <base-sha> <notes-dir> <spec>... | release-push.sh no-credential}

# The identity of the commits this script makes, `tag`'s version bump,
# push-fold's fold and push-built's rebuild: GitHub Actions' own bot.
bot_name='github-actions[bot]'
bot_email='41898282+github-actions[bot]@users.noreply.github.com'

# The git pathspecs of every file that can change the Claude Envoy plugin's
# build, one per line, which push-built reads from the build's base with git.
build_inputs=packages/claude-envoy/scripts/build-inputs.txt

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

# deploy_key_env [<fail>]: make this process's git reach GitHub over SSH with
# the deploy key in $DEPLOY_KEY (the header's push-tag paragraph): the key and
# GitHub's host keys go into a directory only the runner's user can read,
# removed when the script exits, GIT_SSH_COMMAND points ssh at them and nothing
# else, and $remote is the repository's SSH URL on GitHub. Before anything is
# made, an unset or empty $DEPLOY_KEY or $GITHUB_REPOSITORY goes to <fail>
# (fail_error unless given; push-main gives fail_outstanding) as one message
# naming it. A second call returns at once: push-built calls it before its own
# fetch and land_on_main again, and the first call's directory serves both.
deploy_key_env() {
  local fail=${1:-fail_error} key known_hosts
  [ -z "$ssh_dir" ] || return 0
  [ -n "${DEPLOY_KEY:-}" ] || "$fail" "DEPLOY_KEY holds the deploy key to reach GitHub with; it is unset or empty."
  [ -n "${GITHUB_REPOSITORY:-}" ] || "$fail" "GITHUB_REPOSITORY names the repository on GitHub; it is unset or empty."
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

# pushed_tag <tag-prefix> [<package-dir>]: print the release tag an earlier
# attempt pushed for this checkout, or nothing (the header's `tagged`
# paragraph). A tag that is not <tag-prefix>X.Y.Z fails with an ::error::.
pushed_tag() {
  local prefix=$1 package_dir=${2:-} head tags candidate commit shown
  head=$(git rev-parse HEAD)
  # The prefix's tags on HEAD and its descendants, newest version first. Ref
  # names hold no whitespace, so the list splits into one tag per word.
  tags=$(git tag --list "$prefix*" --contains HEAD --sort=-v:refname)
  for candidate in $tags; do
    commit=$(git rev-parse "$candidate^{commit}")
    if [ "$commit" = "$head" ] || {
      [ -n "$package_dir" ] &&
        [ "$(git rev-parse -q --verify "$commit^")" = "$head" ] &&
        [ "$(git diff --name-only "$head" "$commit")" = "$package_dir/package.json" ]
    }; then
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

# fail_outstanding <reason>: fail push-main after the release itself succeeded.
# Writes the ::error:: annotation and, under GitHub Actions, the job summary.
fail_outstanding() {
  local reason=$1
  local message="$reason The release itself succeeded: the package artifacts are published, the tag and the GitHub release exist; only this version-bump push to main is outstanding. main's $manifest keeps the older version until the next release, which computes versions from tags and is unaffected."
  echo "::error::$message"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    printf '**Version bump for %s v%s did not land on main.** %s\n' "$package_dir" "$version" "$message" >> "$GITHUB_STEP_SUMMARY"
  fi
  exit 1
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

# commit_build_on <tip> <base>: commit the build extract_built just wrote over
# <tip>'s tree, as github-actions[bot] with the subject naming <base>, the
# commit the build was made from, and move HEAD to it, leaving the commit in
# $commit, or $commit empty when <tip>'s files already match the build, where
# nothing is committed (the out-parameter build_inputs_changed sets inputs_why
# by). The commit is not printed, because `commit=$(commit_build_on ...)` would
# run it in a subshell, whose EXIT trap removes every on_exit_remove directory,
# the deploy key's among them, before land_on_main pushes. The command runs
# from the repository's root, where extract_built left it.
commit_build_on() {
  local tip=$1 base=$2 index_dir tree tip_tree
  commit=''
  # The two directories are written over the tip's tree in an index of its
  # own, and the tip is never checked out: it may carry another
  # release-push.sh, and a checkout would rewrite this script while bash runs
  # it. Only the two directories can change: the index starts as the tip's
  # tree, and `git add` is limited to them.
  index_dir=$(mktemp -d) || fail_error "making a directory for a temporary index failed"
  on_exit_remove "$index_dir"
  GIT_INDEX_FILE="$index_dir/index" git read-tree "$tip" || fail_error "reading $tip's tree into a temporary index failed"
  GIT_INDEX_FILE="$index_dir/index" git add -A -- packages/claude-envoy/dist packages/claude-envoy/legion ||
    fail_error "staging packages/claude-envoy/dist and packages/claude-envoy/legion over $tip's tree failed"
  tree=$(GIT_INDEX_FILE="$index_dir/index" git write-tree) || fail_error "writing the build's tree failed"
  rm -rf "$index_dir"
  tip_tree=$(git rev-parse "$tip^{tree}") || fail_error "reading $tip's tree failed"
  [ "$tree" != "$tip_tree" ] || return 0
  # The identity `tag` commits as, set for author and committer alike in the
  # environment, which wins over any GIT_AUTHOR_* or GIT_COMMITTER_* already
  # set. The subject names the base, the commit the build was made from, which
  # main_build_base reads back.
  commit=$(GIT_AUTHOR_NAME=$bot_name GIT_AUTHOR_EMAIL=$bot_email GIT_COMMITTER_NAME=$bot_name GIT_COMMITTER_EMAIL=$bot_email \
    git commit-tree --no-gpg-sign -p "$tip" -m "chore(claude-envoy): rebuild dist/ and legion/ from ${base:0:12} [skip ci]" "$tree") ||
    fail_error "committing the build on $tip failed"
  git update-ref --no-deref HEAD "$commit" || fail_error "moving HEAD to $commit failed"
}

# main_build_base <tip>: print the commit main's committed build at <tip> was
# made from, or nothing when main holds no build (the header's push-built
# paragraph). Main's build is the newest commit on <tip>'s first-parent line
# that touches packages/claude-envoy/dist or packages/claude-envoy/legion. When
# github-actions[bot] authored it with exactly push-built's subject, and the 12
# hex there name a commit that is its ancestor, that commit is printed;
# otherwise (a pull request's own committed build, or a subject a person wrote)
# the commit itself, as a build of at most that commit.
main_build_base() {
  local tip=$1 built author subject named
  local rebuild_re='^chore\(claude-envoy\): rebuild dist/ and legion/ from ([0-9a-f]{12}) \[skip ci\]$'
  built=$(git log -1 --first-parent --format=%H "$tip" -- packages/claude-envoy/dist packages/claude-envoy/legion) ||
    fail_error "finding the newest commit on main that touches packages/claude-envoy/dist or packages/claude-envoy/legion failed"
  [ -n "$built" ] || return 0
  author=$(git log -1 --format=%ae "$built") || fail_error "reading the author of $built failed"
  subject=$(git log -1 --format=%s "$built") || fail_error "reading the subject of $built failed"
  if [ "$author" = "$bot_email" ] && [[ $subject =~ $rebuild_re ]] &&
    named=$(git rev-parse --verify --quiet "${BASH_REMATCH[1]}^{commit}") &&
    is_ancestor "$named" "$built"; then
    echo "$named"
  else
    echo "$built"
  fi
}

# build_inputs_changed <base> <tip>: set inputs_why to why a build input changed
# between <base> and <tip>, naming the first changed paths, or to nothing when
# none did (the header's push-built paragraph). The pathspecs are $build_inputs
# as <base> holds it, read with git; a <base> without that file, or with no
# pathspec line in it, counts as changed, with a ::warning naming it.
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

# fold_plan <spec>...: set plan to the fold's plan, the lines fold-plan prints
# (the header's fold-plan paragraph), each ending in a newline; empty when there
# is nothing to fold. Sets groups to its distinct <package-dir><TAB><label>
# pairs, in the plan's order.
fold_plan() {
  [ $# -gt 0 ] || fail_error "no package spec given; each is <package-dir>[=<tag-prefix>]"
  local spec dir prefix index=0 names=' ' head head_label head_date entry meta path commit candidate tag version label date lines=''
  local package_dir_re='^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$'
  local unexpected=()
  head=$(git rev-parse HEAD)
  head_label="main@${head:0:12}"
  head_date=$(commit_date HEAD)
  for spec in "$@"; do
    dir=${spec%%=*} prefix=''
    [[ $spec != *=* ]] || prefix=${spec#*=}
    # The package dir goes into paths and pathspecs, the prefix into a tag
    # pattern, and the dir's basename names the notes file of each group.
    # package_dir_re keeps the dir free of pathspec magic (a leading ':') and
    # of glob characters, so it needs no --literal-pathspecs; a fragment's name
    # is anything git holds, so each git call given one reads it literally.
    if ! [[ $dir =~ $package_dir_re ]] || { [[ $spec == *=* ]] && ! [[ $prefix =~ ^[A-Za-z0-9._-]+$ ]]; }; then
      fail_error "package spec $(quote_all "$spec") is not <package-dir>[=<tag-prefix>], a relative path of letters, digits, '.', '_' and '-' and a prefix of the same without '/'"
    fi
    [[ $names != *" ${dir##*/} "* ]] || fail_error "two package specs name a directory ${dir##*/}; the notes files are named by it, so each must differ"
    names+="${dir##*/} "
    git cat-file -e "HEAD:$dir/CHANGELOG.md" 2>/dev/null || fail_error "$dir holds no CHANGELOG.md at HEAD to fold its fragments into"
    index=$((index + 1))
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
        date=$(commit_date "$tag^{commit}")
      fi
      lines+="$index"$'\t'"$dir"$'\t'"$label"$'\t'"$date"$'\t'"$path"$'\n'
    done < <(git ls-tree -z HEAD -- "$dir/.changes/unreleased/")
  done
  if [ ${#unexpected[@]} -gt 0 ]; then
    mapfile -d '' -t unexpected < <(printf '%s\0' "${unexpected[@]}" | LC_ALL=C sort -z)
    fail_error "$(quote_all "${unexpected[@]}") at HEAD: each entry in a package's .changes/unreleased/ must be a regular file whose name ends in .yaml, a changie fragment, or the fold never folds it; move it, rename it or remove it. Move or rename a fragment in one commit, with git mv: a fragment removed in one commit and added again in another is a new entry, which the release after it labels"
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

# refuse_unlisted <spec>...: fail when HEAD's tree holds a file under the
# .changes/ of a package no <spec> names (the header's fold-plan paragraph).
# It reads HEAD's tree, as fold_plan does, never the working tree, and the
# package a path belongs to is read from the path with a bash regex, so the
# only pathspec is packages/, which holds no magic.
refuse_unlisted() {
  local spec path listed=' '
  local orphans=()
  for spec in "$@"; do
    listed+="${spec%%=*} "
  done
  while IFS= read -r -d '' path; do
    [[ $path =~ ^(packages/[^/]+)/\.changes/ ]] || continue
    [[ $listed == *" ${BASH_REMATCH[1]} "* ]] || orphans+=("$path")
  done < <(git ls-tree -r -z --name-only HEAD -- packages/)
  [ ${#orphans[@]} -gt 0 ] || return 0
  mapfile -d '' -t orphans < <(printf '%s\0' "${orphans[@]}" | LC_ALL=C sort -z)
  fail_error "$(quote_all "${orphans[@]}") at HEAD: each is under the .changes/ of a package the fold was not given, and the fold folds the fragments of the packages it is given alone, here $(quote_all "$@"), so the file would stay on main unfolded; remove it, or give its package a .changie.yaml and a CHANGELOG.md and add it to CHANGELOG_PACKAGES in .github/workflows/changelog-fold.yaml"
}

# group_column <dir> <label> <column>: the plan's <column> (3 the date, 4 the
# path) on each line of that group.
group_column() {
  awk -F '\t' -v dir="$1" -v label="$2" -v column="$3" '$1 == dir && $2 == label { print $column }' <<<"$plan"
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

# has_heading <changelog> <label>: true when a line of <changelog> starts with
# "## [<label>]".
has_heading() {
  awk -v heading="## [$2]" 'index($0, heading) == 1 { found = 1 } END { exit !found }' "$1"
}

# insert_block <changelog> <label> <date> <notes>: write the block of <label>
# into <changelog> where the header's push-fold paragraph places it, keeping
# every existing line as it is.
insert_block() {
  local changelog=$1 label=$2 date=$3 notes_file=$4 at='' number line version heading_re='^## \[([^]]+)\]'
  while IFS=: read -r number line; do
    if [[ $label != main@* && $line =~ $heading_re ]]; then
      version=${BASH_REMATCH[1]}
      if is_plain_version "$version" && version_gt "$version" "$label"; then
        continue
      fi
    fi
    at=$number
    break
  done < <(grep -n '^## ' "$changelog")
  {
    if [ -n "$at" ]; then
      head -n "$((at - 1))" "$changelog"
    elif [ -s "$changelog" ]; then
      cat "$changelog"
      # A last line without its newline gets one before the blank line.
      [ -z "$(tail -c 1 "$changelog")" ] || echo
      echo
    fi
    printf '## [%s] - %s\n\n' "$label" "$date"
    awk 'NF { if (!first) first = NR; last = NR } { line[NR] = $0 } END { for (i = first; i <= last; i++) print line[i] }' "$notes_file"
    echo
    if [ -n "$at" ]; then tail -n "+$at" "$changelog"; fi
  } > "$changelog.fold"
  # Written over the file, not moved onto it, so the file keeps its mode.
  cat "$changelog.fold" > "$changelog"
  rm "$changelog.fold"
}

# land_on_main <onto-sha> <workflow-file>: land HEAD, exactly one commit whose
# one parent is <onto-sha>, on main with the deploy key when main's tip is still
# <onto-sha>, and otherwise push nothing and start a fresh run of
# <workflow-file> on main (the header's land_on_main paragraph, under
# push-fold). push-fold and push-built end with it. Never forces, never
# rebases. Returns 0 when it pushed or started a run, leaving which in $landed
# (pushed or restarted); every other outcome is an ::error:: and exit 1.
land_on_main() {
  local onto=$1 workflow=$2 shas head parents tip why
  landed=''
  # HEAD's sha, then its parents, space-separated: exactly one, onto.
  shas=$(git rev-list --parents -n 1 HEAD) || fail_error "reading HEAD failed"
  head=${shas%% *}
  parents=${shas#"$head"}
  parents=${parents# }
  if [ "$parents" != "$onto" ]; then
    fail_error "HEAD $head has the parents '$parents', not exactly the one parent $onto; only one commit made on $onto lands on main"
  fi
  deploy_key_env
  git fetch "$remote" main || fail_error "fetching main failed"
  tip=$(git rev-parse FETCH_HEAD) || fail_error "reading main's fetched tip failed"
  if [ "$tip" = "$onto" ]; then
    if git push "$remote" HEAD:refs/heads/main; then
      landed=pushed
      return 0
    fi
    git fetch "$remote" main || fail_error "the push to main was rejected, and fetching main again failed"
    tip=$(git rev-parse FETCH_HEAD) || fail_error "reading main's fetched tip failed"
    if [ "$tip" = "$onto" ]; then
      fail_error "the push of $head to main was rejected while main's tip is still $onto, the commit $head was made on, so no newer commit beat it; the push itself failed"
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
    package_dir=${2:?package dir}
    commit_message=${3:?commit message}
    tag=${4:?tag}
    git config user.name "$bot_name"
    git config user.email "$bot_email"
    git add "$package_dir/package.json"
    git diff --staged --quiet || git commit -m "$commit_message"
    git fetch --tags origin
    if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
      tagged_version=$(git show "$tag:$package_dir/package.json" | jq -r .version)
      current_version=$(jq -r .version "$package_dir/package.json")
      if [ "$tagged_version" != "$current_version" ]; then
        echo "::error::tag $tag already exists with version $tagged_version, not $current_version"
        exit 1
      fi
      echo "Tag $tag already exists for version $current_version; reusing it"
    else
      git tag "$tag"
    fi
    ;;
  tagged)
    pushed_tag "${2:?tag prefix}" "${3:-}"
    ;;
  version)
    shift
    usage='usage: release-push.sh version <tag-prefix> [--package-dir <dir>] [--first-npm <npm package>] [--expect <X.Y.Z>] [--patch-path <path>]... -- <paths>...'
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
    package_dir='' first_npm='' expect=''
    bump_args=()
    while [ $# -gt 0 ]; do
      case $1 in
        --package-dir | --first-npm | --expect | --patch-path)
          if [ $# -lt 2 ]; then
            echo "::error::release-push.sh version: $1 needs a value; $usage" >&2
            exit 2
          fi
          case $1 in
            --package-dir) package_dir=$2 ;;
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
    tag=$(pushed_tag "$prefix" "$package_dir")
    if [ -n "$tag" ]; then
      version=${tag#"$prefix"}
      skip=false on_head=true
      log="Releasing $tag, already pushed for this checkout"
    else
      prev_tag=$(git for-each-ref --count=1 --sort=-v:refname --format='%(refname:strip=2)' "refs/tags/$prefix*")
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
  push-main)
    package_dir=${2:?package dir}
    version=${3:?released version}
    manifest="$package_dir/package.json"
    deploy_key_env fail_outstanding
    only_bump="push-main pushes the one version-bump commit, which changes only $manifest, onto main's tip: its parent's $manifest with .version set to $version and nothing else, as JSON; a run from a branch other than main lands nothing but that exact bump on main."
    for attempt in 1 2 3 4 5; do
      git fetch "$remote" main || fail_outstanding "Fetching main from origin failed on attempt $attempt."
      upstream=$(git rev-parse FETCH_HEAD) || fail_outstanding "Reading main's fetched tip failed."
      # What goes over main's tip is the one bump commit, and only it (the
      # header's push-main paragraph), checked before every push so a rebased
      # bump is checked again. HEAD on main itself is what `tag` leaves when
      # the manifest already held the version; it comes before the parent
      # check because a root commit (a repository's first) has no parent.
      extra=$(git rev-list --count "$upstream..HEAD") || fail_outstanding "Reading the bump commit failed."
      if [ "$extra" = 0 ]; then
        echo "Nothing to push: main already holds this checkout"
        exit 0
      fi
      # %h %P: HEAD's short sha, then its parents. A merge commit whose second
      # parent is main's tip counts as one commit over main while its tree is
      # its first parent's; a root commit counts as one too.
      lineage=$(git log -1 --format='%h %P' HEAD) || fail_outstanding "Reading the bump commit failed."
      read -r -a lineage <<<"$lineage"
      parent_count=$(( ${#lineage[@]} - 1 ))
      if [ "$parent_count" != 1 ]; then
        fail_outstanding "HEAD ${lineage[0]} has $parent_count parents; the version-bump commit has one. $only_bump"
      fi
      parent=${lineage[1]}
      carried=$(git log --format='%h %s' "$upstream..HEAD") || fail_outstanding "Reading the bump commit failed."
      if [ "$extra" != 1 ]; then
        fail_outstanding "HEAD carries $extra commits main does not — ${carried//$'\n'/; } — and $only_bump"
      fi
      # The bump's content, against its one parent — main's tip, or an older
      # main commit it is rebased onto below and checked against again — before
      # any push, rebase or superseded-version exit sees it. The paths and the
      # manifest are compared against the parent, the manifest as JSON (jq -S),
      # so the parent's copy with .version set to the released version must be
      # HEAD's copy.
      changed=$(git diff --name-only "$parent" HEAD) || fail_outstanding "Reading the bump commit's paths failed."
      if [ "$changed" != "$manifest" ]; then
        paths=${changed//$'\n'/ }
        fail_outstanding "HEAD carries 1 commit main does not — $carried — which changes ${paths:-nothing}, and $only_bump"
      fi
      parent_copy=$(git show "$parent:$manifest" 2>/dev/null) || fail_outstanding "$manifest is missing at HEAD's parent $parent, so HEAD adds it rather than bumping it. $only_bump"
      expected=$(jq -S --arg v "$version" '.version = $v' <<<"$parent_copy") || fail_outstanding "$manifest at HEAD's parent $parent is not JSON."
      actual=$(git show "HEAD:$manifest" | jq -S .) || fail_outstanding "HEAD's $manifest is missing or not JSON."
      if [ "$expected" != "$actual" ]; then
        fail_outstanding "HEAD's $manifest differs from its parent's in more than .version = $version. $only_bump"
      fi
      if [ "$parent" = "$upstream" ]; then
        # The bump sits on main's tip, checked: push it. HEAD:refs/heads/main
        # serves a detached HEAD and a branch alike; the checks above are what
        # keep everything but the bump off main.
        git push "$remote" HEAD:refs/heads/main && exit 0
        if [ "$attempt" = 5 ]; then
          fail_outstanding "The version-bump commit for $package_dir v$version lost the push race to main 5 times."
        fi
        # main moved between the fetch and the push: its new tip is what to
        # check and rebase onto.
        git fetch "$remote" main || fail_outstanding "Fetching main from origin failed on attempt $attempt."
        upstream=$(git rev-parse FETCH_HEAD) || fail_outstanding "Reading main's fetched tip failed."
      fi
      # HEAD's parent is an older main commit (main moved since the checkout,
      # or the push lost the race): the bump is rebased onto main's tip and the
      # next attempt checks it there. Main's manifest at or past this version
      # needs nothing pushed (the header's push-main paragraph). A manifest
      # main deleted, or one without a plain version, is left to the rebase
      # below.
      main_version=$(git show "$upstream:$manifest" 2>/dev/null | jq -r .version 2>/dev/null) || main_version=""
      if is_plain_version "$main_version" &&
        [ "$(printf '%s\n%s\n' "$version" "$main_version" | sort -V | tail -n 1)" = "$main_version" ]; then
        if [ "$main_version" = "$version" ]; then
          reason="main's $manifest already holds v$version, so this version bump has nothing to add."
        else
          reason="main's $manifest holds v$main_version, a later release than v$version, so this version bump is superseded."
        fi
        echo "::notice title=Version bump not pushed::$reason"
        if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
          printf '**Version bump for %s v%s not pushed.** %s\n' "$package_dir" "$version" "$reason" >> "$GITHUB_STEP_SUMMARY"
        fi
        exit 0
      fi
      if git rebase "$upstream"; then
        continue
      fi
      conflicted=$(git diff --name-only --diff-filter=U) || {
        git rebase --abort 2>/dev/null || true
        fail_outstanding "Rebasing the version-bump commit onto main ($upstream) stopped and its conflicting paths could not be read; the rebase was aborted."
      }
      # Resolvable only when exactly the manifest conflicted and main still has
      # it: a modify/delete conflict lists that same one path.
      if [ "$conflicted" != "$manifest" ] || ! git cat-file -e "HEAD:$manifest" 2>/dev/null; then
        git rebase --abort 2>/dev/null || true
        if [ -z "$conflicted" ]; then
          fail_outstanding "Rebasing the version-bump commit onto main ($upstream) failed without conflicting paths."
        fi
        fail_outstanding "Rebasing the version-bump commit onto main ($upstream) conflicts in: ${conflicted//$'\n'/ }. Only a conflict confined to $manifest, with the file still present on main, is resolved automatically."
      fi
      echo "Resolving the $manifest version-line conflict to v$version"
      # During a rebase stop HEAD is main's side (plus any commit already
      # replayed), so this keeps every other change main made to the file. The
      # rewrite lands through a temp file so a failed pipe never leaves a
      # truncated manifest. The merge backend opens an editor on --continue;
      # give it none.
      if ! { git show "HEAD:$manifest" | jq --arg v "$version" '.version = $v' > "$manifest.tmp" \
          && mv "$manifest.tmp" "$manifest" \
          && git add "$manifest" \
          && GIT_EDITOR=true git rebase --continue; }; then
        rm -f "$manifest.tmp"
        git rebase --abort 2>/dev/null || true
        fail_outstanding "Resolving the $manifest version-line conflict onto main ($upstream) failed; the rebase was aborted."
      fi
    done
    # Only main moving under every attempt gets here: each rebase placed the
    # bump on a tip that was no longer main's by the next fetch.
    fail_outstanding "The version-bump commit for $package_dir v$version could not be placed on main's tip in 5 attempts."
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
    # status, the build input list, main's build and the two directories. The
    # job runs this from the checkout's root, but a run from a subdirectory
    # must land the same commit, where a pathspec read from there would find
    # nothing. extract_built moves to the root as well, and main_build_base
    # and build_inputs_changed run before it. <tar> is the caller's path, so
    # it is made absolute before the move, as extract_built does with its own.
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
    built_base=$(main_build_base "$tip") || exit 1
    if [ -n "$built_base" ] && ! is_ancestor "$built_base" "$base"; then
      echo "::notice title=Nothing pushed; main holds a newer build::main already holds a build of $built_base, newer than $base, the commit this build was made from, so nothing was pushed and no run was started" >&2
      exit 0
    fi
    inputs_why=''
    if [ "$tip" != "$base" ]; then
      build_inputs_changed "$base" "$tip"
    fi
    extract_built "$tar_file"
    commit_build_on "$tip" "$base"
    if [ -z "$commit" ]; then
      echo "::notice title=Nothing to push::main's packages/claude-envoy/dist and packages/claude-envoy/legion at $tip already match this build" >&2
      if [ -n "$inputs_why" ]; then
        fresh_run "$workflow" "$inputs_why" "A fresh run started" "$inputs_why, so"
      fi
      exit 0
    fi
    if [ "$tip" != "$base" ]; then
      echo "::notice title=Landing on main's tip::built from $base, landing on $tip, main's tip, as $commit, which changes only packages/claude-envoy/dist and packages/claude-envoy/legion: main moved since the build started" >&2
    fi
    # land_on_main fetches main again and pushes only when main's tip is still
    # the tip the commit was made on; when main moved again since the fetch
    # above, it starts the one fresh run itself.
    land_on_main "$tip" "$workflow"
    if [ "$landed" = pushed ] && [ -n "$inputs_why" ]; then
      why="the build already landed on main as $commit, but $inputs_why, so main's build is one run behind its inputs"
      fresh_run "$workflow" "$why" "A fresh run started" "the build landed on main as $commit, but $inputs_why, so"
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
    fold_plan "$@"
    refuse_unlisted "$@"
    printf '%s' "$plan"
    ;;
  fold-notes)
    [ $# -ge 3 ] || fail_error "usage: release-push.sh fold-notes <notes-dir> <spec>..."
    notes_dir=${2%/}
    shift 2
    if [ -e "$notes_dir" ] || [ -L "$notes_dir" ]; then
      if [ ! -d "$notes_dir" ] || [ -L "$notes_dir" ] || [ -n "$(ls -A "$notes_dir")" ]; then
        fail_error "$(quote_all "$notes_dir") exists and is not an empty directory; the notes go into a directory of their own"
      fi
    fi
    fold_plan "$@"
    mkdir -p "$notes_dir"
    if [ -z "$plan" ]; then
      echo "Nothing to fold"
      exit 0
    fi
    while IFS=$'\t' read -r dir label; do
      git cat-file -e "HEAD:$dir/.changie.yaml" 2>/dev/null || fail_error "$dir holds no .changie.yaml at HEAD to render its fragments with"
      mapfile -t fragments < <(group_column "$dir" "$label" 4)
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
  push-fold)
    [ $# -ge 4 ] || fail_error "usage: release-push.sh push-fold <base-sha> <notes-dir> <spec>..."
    base=$2 notes_dir=${3%/}
    shift 3
    is_commit_sha "$base" || fail_error "the base $(quote_all "$base") is not a commit's 40 lower-case hex digits"
    head=$(git rev-parse HEAD)
    [ "$head" = "$base" ] || fail_error "HEAD is $head, not the base $base the build job built"
    [ -z "$(git status --porcelain --untracked-files=no)" ] || fail_error "the working tree or the index differs from HEAD; the fold is written onto a clean checkout of the base"
    for var in DEPLOY_KEY GITHUB_REPOSITORY; do
      [ -n "${!var:-}" ] || fail_error "$var is unset or empty, and landing the fold on main needs it; nothing was written"
    done
    fold_plan "$@"
    if [ -z "$plan" ]; then
      echo "Nothing to fold"
      exit 0
    fi
    # A version group whose notes file does not exist at all is deferred (the
    # header's push-fold paragraph). The deferred groups are named here, before
    # the hand-over check, so that check expects the notes of every other group:
    # a version group's notes that exist but are not a regular file, and every
    # main@ group's notes, stay its to refuse.
    names=() deferred=''
    while IFS=$'\t' read -r dir label; do
      name=$(notes_name "$dir" "$label")
      if [[ $label != main@* ]] && [ ! -e "$notes_dir/$name" ] && [ ! -L "$notes_dir/$name" ]; then
        deferred+="$dir"$'\t'"$label"$'\n'
      else
        names+=("$name")
      fi
    done <<<"$groups"
    check_hand_over "$notes_dir" "the fold's plan names no notes for" "${names[@]}"
    if [ -n "$deferred" ]; then
      while IFS=$'\t' read -r dir label; do
        echo "::notice title=Fold deferred::$dir $label is not folded in this run and its fragments stay on main: the build rendered no notes for it, so its release tag is newer than the build; a later run folds it (the release's own completion starts one, and the daily run is the backstop)" >&2
      done < <(printf '%s' "$deferred")
      set_plan "$(printf '%s' "$plan" | awk -F '\t' 'NR == FNR { deferred[$1 FS $2]; next } !(($1 FS $2) in deferred)' <(printf '%s' "$deferred") -)"
      if [ -z "$plan" ]; then
        echo "Nothing to fold in this run: every group of the plan is deferred to a later run"
        exit 0
      fi
    fi
    while IFS=$'\t' read -r dir label; do
      notes_file="$notes_dir/$(notes_name "$dir" "$label")"
      shown=$(quote_all "$notes_file")
      [ "$({ tr -dc '\000' | wc -c; } < "$notes_file")" = 0 ] || fail_error "$shown, the notes of $dir for $label, holds a NUL byte"
      LC_ALL=C grep -q '[^[:space:]]' "$notes_file" || fail_error "$shown, the notes of $dir for $label, holds nothing but blank lines"
      if LC_ALL=C grep -q '^## ' "$notes_file"; then
        fail_error "$shown, the notes of $dir for $label, holds a line starting '## '; the heading is push-fold's to write"
      fi
      if has_heading "$dir/CHANGELOG.md" "$label"; then
        fail_error "$dir/CHANGELOG.md already holds the heading ## [$label]"
      fi
    done <<<"$groups"
    while IFS=$'\t' read -r dir label; do
      date=$(group_column "$dir" "$label" 3 | sed -n 1p)
      insert_block "$dir/CHANGELOG.md" "$label" "$date" "$notes_dir/$(notes_name "$dir" "$label")"
    done <<<"$groups"
    mapfile -t fragments < <(printf '%s' "$plan" | cut -f4)
    mapfile -t changelogs < <(printf '%s\n' "$groups" | cut -f1 | uniq | sed 's|$|/CHANGELOG.md|')
    git --literal-pathspecs rm -q -- "${fragments[@]}"
    git --literal-pathspecs add -- "${changelogs[@]}"
    # The identity through the environment, which outranks any configuration
    # and any identity the job's environment already sets.
    GIT_AUTHOR_NAME=$bot_name GIT_AUTHOR_EMAIL=$bot_email GIT_COMMITTER_NAME=$bot_name GIT_COMMITTER_EMAIL=$bot_email \
      git -c commit.gpgsign=false commit -q --no-verify -m "chore: fold changelog fragments [skip ci]"
    echo "Folded ${#fragments[@]} fragments into ${changelogs[*]}"
    land_on_main "$base" changelog-fold.yaml
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

