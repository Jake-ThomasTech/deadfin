#!/usr/bin/env bash
set -euo pipefail
helper=$(cd "$(dirname "$0")" && pwd)/image-publication
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
git init -q -b master
GIT_COMMITTER_DATE='2026-10-18T07:15:00+02:00' \
  git -c user.name=test -c user.email=test@example.invalid commit -qm base --allow-empty
sha=$(git rev-parse HEAD)
git update-ref refs/remotes/origin/master "$sha"
git tag 0.2.0
git tag v0.2.1
git tag v0.3.0-rc.1
git tag 0.3.0-rc.12
# Master publishes a UTC commit-time tag that no release-line tag can match.
for ref in master refs/heads/master; do
  test "$("$helper" "$ref" | sed -n 's/^publication_tag=//p')" = "master-20261018051500-$(printf '%s' "$sha" | cut -c1-12)"
  test "$("$helper" "$ref" | sed -n 's/^version=//p')" = "dev-$(printf '%s' "$sha" | cut -c1-12)"
  test "$("$helper" "$ref" | sed -n 's/^channel=//p')" = master
  test "$("$helper" "$ref" | sed -n 's/^source_branch=//p')" = master
done
test "$("$helper" train/0.2.0 | sed -n 's/^publication_tag=//p')" = "train-0.2.0-$sha"
test "$("$helper" train/v0.2.3 | sed -n 's/^publication_tag=//p')" = "train-v0.2.3-$sha"
test "$("$helper" train/v0.2.3 | sed -n 's/^version=//p')" = "v0.2.3-dev-$(printf '%s' "$sha" | cut -c1-12)"
test "$("$helper" train/v0.2.3 | sed -n 's/^channel=//p')" = train
test "$("$helper" train/v0.2.3 | tail -n 1)" = "channel=train"
# Release-line branches publish channel dev under a UTC commit-time tag.
dev="0.4.0-dev.20261018051500-$(printf '%s' "$sha" | cut -c1-12)"
for ref in v0.4 refs/heads/v0.4; do
  test "$("$helper" "$ref" | sed -n 's/^publication_tag=//p')" = "$dev"
  test "$("$helper" "$ref" | sed -n 's/^version=//p')" = "$dev"
  test "$("$helper" "$ref" | sed -n 's/^channel=//p')" = dev
  test "$("$helper" "$ref" | sed -n 's/^source_branch=//p')" = v0.4
done
test "$("$helper" v10.12 | sed -n 's/^version=//p')" = "10.12.0-dev.20261018051500-$(printf '%s' "$sha" | cut -c1-12)"
for tag in 0.2.0 v0.2.1; do
  test "$("$helper" "refs/tags/$tag" | sed -n 's/^version=//p')" = "$tag"
  test "$("$helper" "refs/tags/$tag" | sed -n 's/^channel=//p')" = release
done
test "$("$helper" refs/tags/v0.3.0-rc.1 | sed -n 's/^publication_tag=//p')" = v0.3.0-rc.1
test "$("$helper" refs/tags/v0.3.0-rc.1 | sed -n 's/^version=//p')" = 0.3.0-rc.1
test "$("$helper" refs/tags/v0.3.0-rc.1 | sed -n 's/^channel=//p')" = rc
test "$("$helper" refs/tags/v0.3.0-rc.1 | sed -n 's/^source_branch=//p')" = refs/tags/v0.3.0-rc.1
test "$("$helper" refs/tags/0.3.0-rc.12 | sed -n 's/^publication_tag=//p')" = 0.3.0-rc.12
test "$("$helper" refs/tags/0.3.0-rc.12 | sed -n 's/^version=//p')" = 0.3.0-rc.12
test "$("$helper" refs/tags/0.3.0-rc.12 | sed -n 's/^channel=//p')" = rc
for ref in feature/x train/0.2 train/0.3.0-rc.1 v0.4.0 v0.04 v1 v0.4-x vX.Y refs/heads/v0.4.1 refs/tags/v0.02.0 refs/tags/v0.3.0-rc. refs/tags/v0.3.0-RC.3; do
  if "$helper" "$ref" >/dev/null 2>&1; then echo "accepted invalid $ref" >&2; exit 1; fi
done
# Real tags on the master commit: only the tag-shape rule can reject them.
for tag in v0.2.0-rc1 v0.3.0-rc.0 v0.3.0-rc.01 v0.3.0-rc v0.3.0-beta.1 \
  v0.3.0-rc.1.2 v0.3.0-rc.1+build v0.3.0+build v0.03.0-rc.1 vv0.3.0-rc.1 0.3.0-rc.1-rc.2; do
  git tag "$tag"
  if err=$("$helper" "refs/tags/$tag" 2>&1 >/dev/null); then echo "accepted invalid tag $tag" >&2; exit 1; fi
  [[ $err == 'release tag must be '* ]] || { echo "tag $tag rejected for the wrong reason: $err" >&2; exit 1; }
done
git -c user.name=test -c user.email=test@example.invalid commit -qm unreleased --allow-empty
git tag 0.2.2
git tag v0.3.0-rc.2
if "$helper" refs/tags/0.2.2 >/dev/null 2>&1; then echo 'accepted unmerged release' >&2; exit 1; fi
if "$helper" refs/tags/v0.3.0-rc.2 >/dev/null 2>&1; then echo 'accepted unmerged release candidate' >&2; exit 1; fi
if "$helper" refs/tags/0.2.0 >/dev/null 2>&1; then echo 'accepted wrong checkout' >&2; exit 1; fi
if "$helper" refs/tags/v0.3.0-rc.1 >/dev/null 2>&1; then echo 'accepted wrong checkout for release candidate' >&2; exit 1; fi
echo 'image publication identity tests passed'
