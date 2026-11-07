#!/usr/bin/env bash
# Release source-state publisher, shared by the release workflows.
#
# The workflows split each release into a build job, which runs the
# dependencies' code and holds no credential, and a publish job, which holds
# the credentials and runs no dependency code. `version`, `check-built`,
# `extract-built`, `no-credential` and `fold-notes` are the build job's;
# `release`, `release-section`, `restart` and `push-tag` the publish job's;
# `tag`, `tagged`, `pair-version` and `handed` serve both;
# `fold-orphans` and `fold-check` are a pull request's.
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
#            tags. A commit left to another run loses an image and a version
#            number of its own, never its change: the header of
#            .github/workflows/release-envoy-listener.yaml, above its
#            `concurrency:` block, says why, and is the one copy of that
#            reasoning. The run started here is a workflow_dispatch, which
#            that file's workflow-level group keys on its run id, so it is a
#            group of its own and no newer push replaces it while it waits.
# tag:       create the release tag <tag> on $GITHUB_SHA, the commit the
#            release run started on, which main contains, as the legion-v,
#            legion-envoy-v and envoy-infra-v tags always named it; pushes
#            nothing (`push-tag` pushes) and commits nothing. Nothing of a
#            release is committed: the tags record every version, so no file
#            of this checkout is written and a package's committed manifest
#            keeps whatever version was last written to it. Only a publish job
#            runs it: nothing else needs the tag, since fold-notes bounds a
#            release's entries by HEAD, the commit this tags. <version> is the
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
# pair-version: print the version npm carries for a package in this run:
#            <released version> when the caller gives one, else the version of
#            the newest <tag-prefix> tag when that tag's name is
#            <tag-prefix>X.Y.Z, else nothing. The newest is the highest by
#            version order of the name (`--sort=-v:refname`), picked first and
#            checked after: a newest tag no release made prints nothing
#            instead of reaching past it to an older one. release.yaml's
#            legion_build and publish each decide for themselves which build
#            result, skip output and step outcome mean "this run released it"
#            and hand the version over; a package that released nothing in the
#            run carries the version of its newest tag, since a release
#            commits nothing and the tags alone say what was published.
#            A <released version> that is given and is not X.Y.Z is an
#            ::error:: and exit 1, never the newest tag: the caller means a
#            release it just made, and falling through would link the previous
#            plugin with no word said. Nothing printed is a release that did
#            not publish, which both callers refuse.
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
#            removed when the script exits, pushed or not (deploy_key_env).
#            ssh reads no config file,
#            offers only that key, never prompts, and refuses any host key but
#            the pinned ones; GitHub publishes them at `gh api meta --jq
#            .ssh_keys`, which release-push.test.sh compares with these when
#            it can reach GitHub.
# check-built: check <tar>, a build of the Claude Envoy plugin, for what
#            extract-built and the plugin's release accept, holding no
#            credential and writing nothing.
#            packages/claude-envoy/scripts/verify-build.sh runs it (that
#            script's header names the jobs that run it), so a pull request
#            fails for whatever a release's build job would refuse. <tar>
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
#            ::error::. ci.yaml's claude-envoy job runs it to pack the build
#            into the pull request's tarball, in its own disposable checkout.
#            Neither directory is tracked, and nothing a person runs locally
#            writes them.
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
# fold-orphans: fail when HEAD's tree holds a file under the .changes/ of a
#            package no <package-dir> names: one ::error:: naming every such
#            path, sorted, the directories it was given and what to do (remove
#            the file, or give its package a .changie.yaml and a CHANGELOG.md
#            and add it to .github/changelog-packages), and exit 1; nothing is
#            printed and the exit is 0 when HEAD holds no such file. It reads
#            HEAD's tree alone - no history, no tag, no working tree, no
#            CHANGELOG.md - so a shallow checkout without the tags is enough
#            for it. ci.yaml's lint job runs it over
#            .github/changelog-packages on every pull request, inside a
#            required check: a fragment of a package no release and no
#            documentation page renders is refused there. Each <package-dir>
#            is a relative path of letters, digits, '.', '_' and '-'; no
#            directory at all, or one of another shape, is refused.
# fold-notes: render one release of one package: the changelog entries
#            <version> ships, with changie, which is dependency code, into
#            <notes-file>. <package-dir> is the package and <tag-prefix> the
#            tags of its own release line, which bound the release: a package
#            with no release line ships no release notes.
#            The entries are the fragments directly in
#            <package-dir>/.changes/unreleased/ that HEAD holds (git's tree,
#            never the working tree) whose first-adding commit is in the
#            release's group: the commits HEAD holds that no lower
#            <tag-prefix>X.Y.Z tag holds (`git rev-list HEAD --not <every
#            lower tag>`), which are exactly the commits the release's own tag
#            will contain and no release before it did. HEAD is the commit
#            that tag names, so nothing need be tagged before this runs, and
#            other names under the prefix are no releases and bound nothing.
#            The first-adding commit is the one that gave the file its name,
#            following renames: a commit that deletes a file and adds one git
#            pairs with it as a rename, as `git mv` does, renames the entry; a
#            fragment deleted in one commit and added again in another is a
#            new entry, and a copy is never followed. A fragment's path is one
#            file to git, never a pattern: a name holding a glob character
#            gets the commit that added that very file.
#            Each entry directly in the unreleased directory must be a regular
#            file (mode 100644 or 100755) whose name ends in .yaml: any other
#            entry, such as a subdirectory, a symlink, a submodule or a .yml
#            file, is rendered nowhere, so the command fails with one
#            ::error:: naming every such entry, sorted, and tells the author
#            to move or rename a fragment in one commit with `git mv`.
#            A release whose group holds no fragment writes no file. Otherwise
#            the group's fragments are rendered by `changie batch 0.0.0` in a
#            directory of their own beside a copy of
#            <package-dir>/.changie.yaml, as HEAD holds both, and the notes
#            become <notes-file>. A package holding no .changie.yaml at HEAD
#            is an ::error::, and so is a fragment changie refuses, naming the
#            package, the version, each fragment changie refuses on its own
#            and changie's output.
#            Stdout holds one line on exit 0 and nothing otherwise, as
#            `version` holds its three: `notes=true` when <notes-file> was
#            written and is not empty, else `notes=false`, which the caller
#            appends to $GITHUB_OUTPUT. Every log line goes to stderr.
# fold-check: render every changelog fragment HEAD holds under each
#            <package-dir>'s .changes/unreleased/ with changie, writing
#            nothing, and fail with one ::error:: naming every fragment
#            changie refuses, each with what changie said about it; a package
#            with no fragment renders nothing, and no fragment at all exits 0.
#            This is the pull request's check that every fragment renders:
#            nothing renders a fragment between the merge and the release that
#            first ships it, and a fragment changie refuses - one of a kind
#            the package's .changie.yaml does not list, say - would fail that
#            release's build job, and through the publish job's pair check the
#            legion release and the image attestation, on every run until a
#            fix merges. The fragments and their refusals are fold-notes', and
#            each <package-dir> is one of fold-orphans'. changie is what
#            renders them, so the command looks for it before anything else
#            and refuses naming it where it is not on PATH. ci.yaml's test job
#            runs it over .github/changelog-packages, beside the changie its
#            release-push.test.sh cases need.
set -euo pipefail

command=${1:?usage: release-push.sh version <tag-prefix> [--first-npm <npm package>] [--expect <X.Y.Z>] [--patch-path <path>]... -- <paths>... | release-push.sh restart <workflow-file> <lost-races> | release-push.sh tag <version> <tag> | release-push.sh tagged <tag-prefix> | release-push.sh pair-version <tag-prefix> [<released version>] | release-push.sh release [--notes <text>] [--upload-to-published] <tag> <title> <asset>... | release-push.sh release-section <tag> <heading> <content-file> | release-push.sh push-tag <tag> | release-push.sh check-built <tar> | release-push.sh extract-built <tar> | release-push.sh handed <tag-prefix> <version> <dir> <file>... | release-push.sh fold-orphans <package-dir>... | release-push.sh fold-notes <version> <notes-file> <package-dir> <tag-prefix> | release-push.sh fold-check <package-dir>... | release-push.sh no-credential}

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
# other directory given here, under the one EXIT trap they share (the deploy
# key's directory and release-section's).
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
# naming it. A second call returns at once, and the first call's directory
# serves every caller.
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

# version_gt <a> <b>: true when version <a> is higher than <b> (`sort -V`).
version_gt() { [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n 1)" = "$1" ]; }

# newest_tag <tag-prefix>: print the newest <tag-prefix> tag, the highest by
# version order of its whole name, or nothing when there is none. The name is
# whatever its pusher chose: `pair-version` prints the version of this tag
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

# check_built <tar>: refuse <tar> unless it is a build the plugin's release
# accepts (the header's check-built paragraph), reading only its listing;
# every refusal is an
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

# A package directory, which fold-orphans, fold-notes and fold-check all take.
# It goes into paths and pathspecs, so package_dir_re keeps it free of
# pathspec magic (a leading ':') and of glob characters and it needs no
# --literal-pathspecs; a fragment's name is anything git holds, so each git
# call given one reads it literally.
package_dir_re='^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$'

# A tag prefix, which `version` and fold-notes both take. It names the tags to
# list and becomes part of the tag a release is given, so it is letters,
# digits, '.', '_' and '-' and nothing else.
tag_prefix_re='^[A-Za-z0-9._-]+$'

# check_package_dirs <package-dir>...: fail unless at least one <package-dir>
# is given and each is a relative path of letters, digits, '.', '_' and '-'.
check_package_dirs() {
  local dir
  [ $# -gt 0 ] || fail_error "no package directory given; each is a relative path of letters, digits, '.', '_' and '-'"
  for dir in "$@"; do
    if ! [[ $dir =~ $package_dir_re ]]; then
      fail_error "package directory $(quote_all "$dir") is not a relative path of letters, digits, '.', '_' and '-'"
    fi
  done
}

# The commits of the release fold-notes renders, one key each, which
# group_commits fills. Declared here, with its type: `in_group=()` on an
# undeclared name would make an indexed array, where a commit sha is read as
# arithmetic.
declare -A in_group=()

# group_commits <tag-prefix> <version>: set in_group to the commits the release
# <version> ships: the commits HEAD holds that no lower <tag-prefix>X.Y.Z tag
# holds. HEAD is the commit the release tags, so these are exactly the commits
# that tag will contain and no release before it did, whether or not it exists
# yet; other names under the prefix are no releases and bound nothing, and with
# no lower tag at all the group is everything HEAD holds, as a first release's
# tag would contain it. Reading them in one `git rev-list` is what keeps a
# release's build job from walking every tag of the prefix once per fragment,
# and every fragment a release ever shipped stays under .changes/unreleased/.
group_commits() {
  local prefix=$1 version=$2 candidate tag_version commit
  local -a lower=()
  in_group=()
  for candidate in $(git tag --list "$prefix*"); do
    tag_version=${candidate#"$prefix"}
    is_plain_version "$tag_version" || continue
    version_gt "$version" "$tag_version" || continue
    lower+=("$candidate")
  done
  while IFS= read -r commit; do
    in_group[$commit]=1
  done < <(git rev-list HEAD --not "${lower[@]}")
}

# fragments_of <package-dir>: set fragments to the changelog fragments of
# <package-dir> that HEAD holds, in name order, which fold-notes and fold-check
# both render: the entries directly in <package-dir>/.changes/unreleased/
# (git's tree, never the working tree) that are regular .yaml files. Any other
# entry there is rendered nowhere, so one ::error:: names every such entry of
# the package, sorted, and the command fails.
fragments_of() {
  local dir=$1 entry meta path
  local -a unexpected=()
  fragments=()
  # No -r, so a subdirectory is one tree entry, refused below with the rest.
  while IFS= read -r -d '' entry; do
    meta=${entry%%$'\t'*} path=${entry#*$'\t'}
    if ! [[ $meta == 100644\ blob\ * || $meta == 100755\ blob\ * ]] || [[ $path != *.yaml ]]; then
      unexpected+=("$path")
      continue
    fi
    [[ $path != *[$'\t\n']* ]] || fail_error "fragment $(quote_all "$path") has a tab or a newline in its name; rename it"
    fragments+=("$path")
  done < <(git ls-tree -z HEAD -- "$dir/.changes/unreleased/")
  if [ ${#unexpected[@]} -gt 0 ]; then
    mapfile -d '' -t unexpected < <(printf '%s\0' "${unexpected[@]}" | LC_ALL=C sort -z)
    fail_error "$(quote_all "${unexpected[@]}") at HEAD: each entry in a package's .changes/unreleased/ must be a regular file whose name ends in .yaml, a changie fragment, or no release renders it; move it, rename it or remove it. Move or rename a fragment in one commit, with git mv: a fragment removed in one commit and added again in another is a new entry, which the release after it ships"
  fi
}

# unlisted_files <package-dir>...: set unlisted to every path HEAD's tree holds
# under the .changes/ of a package no <package-dir> names, sorted; an empty
# array when there is none. fold-orphans refuses them (the header's paragraph
# for it). It reads HEAD's tree, as fragments_of does, never the working tree,
# and the package a path belongs to is read from the path with a bash regex, so
# the only pathspec is packages/, which holds no magic.
unlisted_files() {
  local dir path listed=' '
  unlisted=()
  for dir in "$@"; do
    listed+="$dir "
  done
  while IFS= read -r -d '' path; do
    [[ $path =~ ^(packages/[^/]+)/\.changes/ ]] || continue
    [[ $listed == *" ${BASH_REMATCH[1]} "* ]] || unlisted+=("$path")
  done < <(git ls-tree -r -z --name-only HEAD -- packages/)
  [ ${#unlisted[@]} -eq 0 ] || mapfile -d '' -t unlisted < <(printf '%s\0' "${unlisted[@]}" | LC_ALL=C sort -z)
}

# unlisted_message <package-dir>...: what fold-orphans' ::error:: says about the
# paths unlisted_files left in unlisted: every one of them, the directories the
# command was given, and what to do with the file.
unlisted_message() {
  printf '%s at HEAD: each is under the .changes/ of a package this command was not given, here %s, and it reads the fragments of the packages it is given alone, so no release renders it and it stays where it is; remove it, or give its package a .changie.yaml and a CHANGELOG.md and add it to .github/changelog-packages' "$(quote_all "${unlisted[@]}")" "$(quote_all "$@")"
}

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
  pair-version)
    [ $# -ge 2 ] && [ $# -le 3 ] ||
      fail_error "usage: release-push.sh pair-version <tag-prefix> [<released version>]"
    prefix=$2
    released=${3:-}
    if [ -n "$released" ]; then
      # The version the caller says this run released, which wins. A value
      # that is not X.Y.Z is refused rather than falling through to the newest
      # tag: the caller means a release it just made, and the fall-through
      # would link the previous plugin with no word said.
      is_plain_version "$released" ||
        fail_error "the released version $(quote_all "$released") is not X.Y.Z, each field a number without leading zeros; refusing it"
      printf '%s\n' "$released"
    else
      # A package that released nothing in the run hands over nothing, and
      # carries the version of its newest tag, which npm has. The newest tag
      # decides, and is checked after: a name no release made prints nothing
      # rather than reaching past it to an older tag.
      tag=$(newest_tag "$prefix")
      version=${tag#"$prefix"}
      if is_plain_version "$version"; then
        printf '%s\n' "$version"
      fi
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
    if ! [[ $prefix =~ $tag_prefix_re ]]; then
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
  fold-orphans)
    shift
    check_package_dirs "$@"
    unlisted_files "$@"
    [ ${#unlisted[@]} -eq 0 ] || fail_error "$(unlisted_message "$@")"
    ;;
  fold-notes)
    shift
    [ $# -eq 4 ] ||
      fail_error "usage: release-push.sh fold-notes <version> <notes-file> <package-dir> <tag-prefix>"
    version=$1 notes_file=$2 dir=$3 prefix=$4
    is_plain_version "$version" ||
      fail_error "the release version $(quote_all "$version") is not X.Y.Z, each field a number without leading zeros; refusing it"
    check_package_dirs "$dir"
    [[ $prefix =~ $tag_prefix_re ]] ||
      fail_error "tag prefix $(quote_all "$prefix") holds more than letters, digits, '.', '_' and '-'; refusing it"
    fragments_of "$dir"
    group_commits "$prefix" "$version"
    # The release's own entries: the fragments the commits of this release
    # first added, which is what its tag will hold and no tag before it does.
    group=()
    for fragment in "${fragments[@]}"; do
      commit=$(first_added "$fragment")
      [ -z "${in_group[$commit]:-}" ] || group+=("$fragment")
    done
    if [ ${#group[@]} -eq 0 ]; then
      echo "No changelog fragment of $dir is new in $version; wrote no notes file" >&2
      printf 'notes=false\n'
      exit 0
    fi
    git cat-file -e "HEAD:$dir/.changie.yaml" 2>/dev/null ||
      fail_error "$dir holds no .changie.yaml at HEAD to render its fragments with"
    if ! render_notes "$dir" "$notes_file" "${group[@]}"; then
      # changie does not name the fragment of every refusal (an unknown kind):
      # each fragment is rendered alone to find the ones it refuses.
      output=$changie_output
      refused=()
      for fragment in "${group[@]}"; do
        render_notes "$dir" '' "$fragment" || refused+=("$fragment")
      done
      if [ ${#refused[@]} -gt 0 ]; then
        output="refused alone: $(quote_all "${refused[@]}"); changie said: $output"
      fi
      fail_error "changie could not render $dir's notes for $version; ${output//$'\n'/ }"
    fi
    echo "Wrote $notes_file from ${#group[@]} fragments" >&2
    # The one line the caller appends to $GITHUB_OUTPUT: a render that left an
    # empty file is a release with nothing to upload, as no file is.
    if [ -s "$notes_file" ]; then printf 'notes=true\n'; else printf 'notes=false\n'; fi
    ;;
  fold-check)
    shift
    # Every fragment below is rendered with changie, so a missing changie is
    # one refusal naming it: rendering without it would report each fragment
    # refused, with `changie: command not found` inside the message.
    command -v changie >/dev/null ||
      fail_error "changie is not on PATH, and this command renders every fragment with it; .github/actions/install-changie installs the version the packages' .changie.yaml files are written for"
    check_package_dirs "$@"
    refused=()
    for dir in "$@"; do
      fragments_of "$dir"
      [ ${#fragments[@]} -gt 0 ] || continue
      git cat-file -e "HEAD:$dir/.changie.yaml" 2>/dev/null ||
        fail_error "$dir holds no .changie.yaml at HEAD to render its fragments with"
      if ! render_notes "$dir" '' "${fragments[@]}"; then
        # changie does not name the fragment of every refusal (an unknown
        # kind): each is rendered alone to find which ones it refuses and what
        # it says about each.
        batch=${changie_output//$'\n'/ }
        named=false
        for fragment in "${fragments[@]}"; do
          if ! render_notes "$dir" '' "$fragment"; then
            refused+=("$(quote_all "$fragment"), where changie said: ${changie_output//$'\n'/ }")
            named=true
          fi
        done
        # Every fragment renders alone but not together: changie's own words
        # about the package are all there is to name.
        "$named" || refused+=("every fragment of $dir, which render one by one but not together, where changie said: $batch")
      fi
    done
    if [ ${#refused[@]} -gt 0 ]; then
      joined=$(printf '%s; ' "${refused[@]}")
      fail_error "changie refuses a changelog fragment of this checkout, and the release that first ships it would fail its build job: ${joined%; }"
    fi
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

