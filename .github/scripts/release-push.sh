#!/usr/bin/env bash
# Release source-state publisher, shared by the release workflows.
#
# tag:       commit the version bump and create/push the release tag on that
#            un-rebased commit, so the tag always describes exactly the source
#            the artifacts were built and published from. A pre-existing tag is
#            reused only when it carries the same package version; any other
#            collision fails loudly.
# push-main: push the bump commit to main with a rebase retry. This runs last,
#            after the tag and GitHub release exist: a lost push race only
#            loses the metadata bump on main, which the tag-anchored version
#            computation heals on the next release. Raced-in commits stay
#            outside the tag and are picked up by the next version calculation.
set -euo pipefail

command=${1:?usage: release-push.sh tag <package-dir> <commit-message> <tag> | release-push.sh push-main}

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
    for attempt in 1 2 3 4 5; do
      git push origin main && exit 0
      if [ "$attempt" = 5 ]; then
        echo "::error::version-bump commit lost the push race to main 5 times; the tag and release are already published and self-consistent"
        exit 1
      fi
      git pull --rebase origin main
    done
    ;;
  *)
    echo "::error::unknown release-push command: $command"
    exit 1
    ;;
esac
