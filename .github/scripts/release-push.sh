#!/usr/bin/env bash
# Release source-state publisher, shared by the release workflows.
#
# tag:       commit the version bump and create/push the release tag on that
#            un-rebased commit, so the tag always describes exactly the source
#            the artifacts were built and published from. A pre-existing tag is
#            reused only when it carries the same package version; any other
#            collision fails loudly.
# tagged:    print the release tag an earlier attempt pushed for this checkout,
#            or nothing: the newest <tag-prefix> tag on HEAD itself, or, given
#            <package-dir>, on a child of HEAD that changes only
#            <package-dir>/package.json, the bump commit `tag` makes (the tag
#            sits on HEAD itself when the manifest already held the version and
#            `tag` had nothing to commit). Without <package-dir> only a tag on
#            HEAD counts, as for a job that tags the commit it checked out. A
#            re-run of a release job checks out the same commit, with every tag
#            its first attempt pushed; its version step reads this to release
#            that tag again, where computing the next version from the tag
#            would find an empty range and skip. The version is the tag's name,
#            which anyone who can push a tag chooses, so that tag must be
#            <tag-prefix>X.Y.Z, each field a number without leading zeros: any
#            other name fails with an ::error:: on stderr naming it.
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
# push-main: push the bump commit to main, fetching main and rebasing onto it
#            between attempts, five attempts in all. This runs last, after the
#            tag and GitHub release exist: a lost push only delays the metadata
#            bump on main, which the tag-anchored version computation heals on
#            the next release. Raced-in commits stay outside the tag and are
#            picked up by the next version calculation.
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
#            After a rejected push, push-main reads main's manifest before it
#            rebases. When that manifest already holds the version being
#            released or a later one, there is nothing to push: a re-run whose
#            first attempt landed this bump, or a re-run of an older run after a
#            later release of the package landed. It says so in a notice and
#            exits 0, dropping the bump commit whole, which is right only
#            because that commit changes nothing but the manifest's version. A
#            first push that fast-forwards main is not checked: it can lower
#            main's manifest only when main held a version above the newest
#            tag, which only a hand edit makes, and the tags decide every next
#            version anyway.
# push-tag:  push the local tag <tag> to $GITHUB_REPOSITORY on GitHub over SSH
#            with the deploy key in $DEPLOY_KEY, for a job whose checkout keeps
#            no credential. The key and GitHub's host keys, pinned below, go
#            into a directory of its own that only the runner's user can read,
#            removed when push-tag exits, pushed or not. ssh reads no config
#            file, offers only that key, never prompts, and refuses any host
#            key but the pinned ones; GitHub publishes them at
#            `gh api meta --jq .ssh_keys`, which release-push.test.sh compares
#            with these when it can reach GitHub.
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
set -euo pipefail

command=${1:?usage: release-push.sh tag <package-dir> <commit-message> <tag> | release-push.sh tagged <tag-prefix> [<package-dir>] | release-push.sh release [--notes <text>] [--upload-to-published] <tag> <title> <asset>... | release-push.sh push-main <package-dir> <version> | release-push.sh push-tag <tag> | release-push.sh handed <tag-prefix> <version> <dir> <file>...}

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

# github_known_hosts: GitHub's SSH host keys, the only ones push-tag accepts.
github_known_hosts() {
  cat <<'KNOWN_HOSTS'
github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl
github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=
github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=
KNOWN_HOSTS
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
      git push origin "refs/tags/$tag"
    fi
    ;;
  tagged)
    prefix=${2:?tag prefix}
    package_dir=${3:-}
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
          echo "::error::release-push.sh tagged: $shown, the release tag for this checkout, is not ${prefix}X.Y.Z; refusing it" >&2
          exit 1
        fi
        echo "$candidate"
        exit 0
      fi
    done
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
    for attempt in 1 2 3 4 5; do
      git push origin main && exit 0
      if [ "$attempt" = 5 ]; then
        fail_outstanding "The version-bump commit for $package_dir v$version lost the push race to main 5 times."
      fi
      git fetch origin main || fail_outstanding "Fetching main from origin failed on attempt $attempt."
      upstream=$(git rev-parse FETCH_HEAD)
      # Main's manifest at or past this version needs nothing pushed (the
      # header's push-main paragraph). A manifest main deleted, or one without a
      # plain version, is left to the rebase below.
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
      conflicted=$(git diff --name-only --diff-filter=U)
      # Resolvable only when exactly the manifest conflicted and main still has
      # it: a modify/delete conflict lists that same one path.
      if [ "$conflicted" != "$manifest" ] || ! git cat-file -e "HEAD:$manifest" 2>/dev/null; then
        git rebase --abort 2>/dev/null || true
        if [ -z "$conflicted" ]; then
          fail_outstanding "Rebasing the version-bump commit onto main ($upstream) failed without conflicting paths."
        fi
        fail_outstanding "Rebasing the version-bump commit onto main ($upstream) conflicts in: $(printf '%s' "$conflicted" | tr '\n' ' '). Only a conflict confined to $manifest, with the file still present on main, is resolved automatically."
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
    ;;
  push-tag)
    tag=${2:?tag}
    : "${DEPLOY_KEY:?DEPLOY_KEY holds the deploy key push-tag pushes with}"
    : "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY names the repository push-tag pushes to}"
    ssh_dir=$(mktemp -d)
    trap 'rm -rf "$ssh_dir"' EXIT
    (
      umask 077
      printf '%s\n' "$DEPLOY_KEY" > "$ssh_dir/key"
      github_known_hosts > "$ssh_dir/known_hosts"
    )
    printf -v key '%q' "$ssh_dir/key"
    printf -v known_hosts '%q' "$ssh_dir/known_hosts"
    GIT_SSH_COMMAND="ssh -F /dev/null -i $key -o IdentitiesOnly=yes -o BatchMode=yes -o UserKnownHostsFile=$known_hosts -o GlobalKnownHostsFile=/dev/null -o StrictHostKeyChecking=yes" \
      git push "git@github.com:$GITHUB_REPOSITORY.git" "refs/tags/$tag"
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
  *)
    echo "::error::unknown release-push command: $command"
    exit 1
    ;;
esac

