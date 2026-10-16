#!/usr/bin/env bash
# Release source-state publisher, shared by the release workflows.
#
# The workflows split each release into a build job, which runs the
# dependencies' code and holds no credential, and a publish job, which holds
# the credentials and runs no dependency code. `version` and `no-credential`
# are the build job's; `tag`, `release`, `push-main` and `push-tag` the
# publish job's; `tagged` and `handed` serve both.
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
#            handed it: when the tags now give another version, or nothing to
#            release, another run took the tag first, and the error says to
#            re-run the whole run. Every log line, notice and error goes to
#            stderr, so stdout is the three lines or, on any failure, nothing.
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
#            which push-main shares). ssh reads no config file, offers only
#            that key, never prompts, and refuses any host key but the pinned
#            ones; GitHub publishes them at `gh api meta --jq .ssh_keys`, which
#            release-push.test.sh compares with these when it can reach GitHub.
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
set -euo pipefail

command=${1:?usage: release-push.sh version <tag-prefix> [--package-dir <dir>] [--first-npm <npm package>] [--expect <X.Y.Z>] [--patch-path <path>]... -- <paths>... | release-push.sh tag <package-dir> <commit-message> <tag> | release-push.sh tagged <tag-prefix> [<package-dir>] | release-push.sh release [--notes <text>] [--upload-to-published] <tag> <title> <asset>... | release-push.sh push-main <package-dir> <version> | release-push.sh push-tag <tag> | release-push.sh handed <tag-prefix> <version> <dir> <file>... | release-push.sh no-credential}

# is_plain_version <value>: true when <value> is X.Y.Z, each field a number
# without leading zeros (release-bump.sh does arithmetic on the fields, where a
# leading zero reads as octal).
is_plain_version() { [[ $1 =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; }

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

# deploy_key_env [<fail>]: make this process's git reach GitHub over SSH with
# the deploy key in $DEPLOY_KEY (the header's push-tag paragraph): the key and
# GitHub's host keys go into a directory only the runner's user can read,
# removed when the script exits, GIT_SSH_COMMAND points ssh at them and nothing
# else, and $remote is the repository's SSH URL on GitHub. Before anything is
# made, an unset or empty $DEPLOY_KEY or $GITHUB_REPOSITORY goes to <fail>
# (fail_error unless given; push-main gives fail_outstanding) as one message
# naming it.
deploy_key_env() {
  local fail=${1:-fail_error} key known_hosts
  [ -n "${DEPLOY_KEY:-}" ] || "$fail" "DEPLOY_KEY holds the deploy key to reach GitHub with; it is unset or empty."
  [ -n "${GITHUB_REPOSITORY:-}" ] || "$fail" "GITHUB_REPOSITORY names the repository on GitHub; it is unset or empty."
  ssh_dir=$(mktemp -d)
  trap 'rm -rf "$ssh_dir"' EXIT
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

case "$command" in
  tag)
    package_dir=${2:?package dir}
    commit_message=${3:?commit message}
    tag=${4:?tag}
    git config user.name "github-actions[bot]"
    git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
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
    summary_line=''
    tag=$(pushed_tag "$prefix" "$package_dir")
    if [ -n "$tag" ]; then
      version=${tag#"$prefix"}
      skip=false
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
      # command's, with nothing on stdout.
      next=$("$(dirname "${BASH_SOURCE[0]}")/release-bump.sh" "$prev_version" "$range" "${bump_args[@]}" -- "$@")
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
        echo "::error::release-push.sh version: the build job handed v$expect; the tags now give $result: another run took the tag first. Re-run the whole run (gh run rerun ${GITHUB_RUN_ID:-<run id>}), not only the failed jobs: the build job must compute the version again" >&2
        exit 1
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
    for file in "$@"; do
      if [ -z "$file" ] || [ "$file" = . ] || [ "$file" = .. ] || [[ $file == */* ]]; then
        echo "::error::release-push.sh handed: $(quote_all "$file") is not a file name: each <file> is a name in <dir>, with no /" >&2
        exit 1
      fi
    done
    if [ ! -d "$dir" ] || [ -L "$dir" ]; then
      echo "::error::release-push.sh handed: $(quote_all "$dir") is not a directory" >&2
      exit 1
    fi

    missing=()
    irregular=()
    for file in "$@"; do
      if [ -L "$dir/$file" ] || { [ -e "$dir/$file" ] && [ ! -f "$dir/$file" ]; }; then
        irregular+=("$file")
      elif [ ! -e "$dir/$file" ]; then
        missing+=("$file")
      fi
    done
    # Everything under <dir>, at any depth, sorted so a refusal names it the same way every time.
    listing=$(mktemp)
    trap 'rm -f "$listing"' EXIT
    find "$dir" -mindepth 1 -print0 > "$listing"
    sort -z -o "$listing" "$listing"
    extra=()
    while IFS= read -r -d '' entry; do
      entry=${entry#"$dir"/}
      listed=false
      for file in "$@"; do
        [ "$file" != "$entry" ] || listed=true
      done
      "$listed" || extra+=("$entry")
    done < "$listing"

    refused=false
    if [ ${#missing[@]} -gt 0 ]; then
      echo "::error::release-push.sh handed: $(quote_all "$dir") lacks $(quote_all "${missing[@]}")" >&2
      refused=true
    fi
    for file in "${irregular[@]}"; do
      echo "::error::release-push.sh handed: $(quote_all "$file") in $(quote_all "$dir") is not a regular file" >&2
      refused=true
    done
    if [ ${#extra[@]} -gt 0 ]; then
      echo "::error::release-push.sh handed: $(quote_all "$dir") holds what the release does not ship: $(quote_all "${extra[@]}")" >&2
      refused=true
    fi
    if "$refused"; then
      exit 1
    fi
    printf '%s\n' "$prefix$version"
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

