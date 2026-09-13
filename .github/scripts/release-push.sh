#!/usr/bin/env bash
# Release source-state publisher, shared by the release workflows.
#
# tag:       commit the version bump and create/push the release tag on that
#            un-rebased commit, so the tag always describes exactly the source
#            the artifacts were built and published from. A pre-existing tag is
#            reused only when it carries the same package version; any other
#            collision fails loudly.
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
#            released (each run derives its version from the newest tag, so the
#            release in flight is always the newest for its package), continue
#            the rebase, and push again. Any other conflicted path is a real
#            divergence: the rebase is aborted and the step fails naming the
#            paths. Exhausting the attempts fails the step too. Every failure
#            here says, in the log and in the job summary, that the publish,
#            tag and GitHub release already succeeded and only this push to
#            main is outstanding.
set -euo pipefail

command=${1:?usage: release-push.sh tag <package-dir> <commit-message> <tag> | release-push.sh push-main <package-dir> <version>}

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
  push-main)
    package_dir=${2:?package dir}
    version=${3:?released version}
    manifest="$package_dir/package.json"
    for attempt in 1 2 3 4 5; do
      git push origin main && exit 0
      if [ "$attempt" = 5 ]; then
        fail_outstanding "The version-bump commit for $package_dir v$version lost the push race to main 5 times."
      fi
      git fetch origin main
      upstream=$(git rev-parse FETCH_HEAD)
      if git rebase "$upstream"; then
        continue
      fi
      conflicted=$(git diff --name-only --diff-filter=U)
      if [ "$conflicted" != "$manifest" ]; then
        git rebase --abort || true
        if [ -z "$conflicted" ]; then
          fail_outstanding "Rebasing the version-bump commit onto main ($upstream) failed without conflicting paths."
        fi
        fail_outstanding "Rebasing the version-bump commit onto main ($upstream) conflicts in: $(printf '%s' "$conflicted" | tr '\n' ' '). Only a conflict confined to $manifest is resolved automatically."
      fi
      echo "Resolving the $manifest version-line conflict to v$version"
      # During a rebase stop HEAD is main's side (plus any commit already
      # replayed), so this keeps every other change main made to the file.
      # The merge backend opens an editor on --continue; give it none.
      git show "HEAD:$manifest" | jq --arg v "$version" '.version = $v' > "$manifest"
      git add "$manifest"
      GIT_EDITOR=true git rebase --continue
    done
    ;;
  *)
    echo "::error::unknown release-push command: $command"
    exit 1
    ;;
esac

