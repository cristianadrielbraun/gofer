#!/usr/bin/env bash
# Publishes a Gofer release end to end, in the order the pieces depend on:
#
#   1. bump the Helm chart (appVersion and chart version) and commit on master
#   2. push a signed vX.Y.Z tag, which starts the Docker image build
#   3. build the release archives in a worktree of the tag
#   4. create a draft GitHub release with the notes and archives
#   5. wait for the Docker image and check it can be pulled
#   6. push a signed chart-vA.B.C tag, which publishes the chart
#   7. publish the release
#
# Usage: scripts/release.sh vX.Y.Z NOTES.md
#
# Steps that already happened are skipped, so after a failure fix the cause
# and run the same command again. CHART_VERSION overrides the chart version,
# which otherwise gets a patch bump.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VERSION="${1:-}"
NOTES="${2:-}"
CHART="charts/gofer/Chart.yaml"

step() { printf '\n==> %s\n' "$*"; }
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }

[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || fail "usage: $0 vX.Y.Z NOTES.md"
[[ -s "$NOTES" ]] || fail "release notes file '$NOTES' is missing or empty"
NOTES="$(cd "$(dirname "$NOTES")" && pwd)/$(basename "$NOTES")"
APP_VERSION="${VERSION#v}"

for tool in git gh curl; do
  command -v "$tool" >/dev/null || fail "$tool is required"
done
gh auth status >/dev/null 2>&1 || fail "gh is not signed in; run: gh auth login"
REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner)"

# The archives need the Go version go.mod asks for. Use a golang.org/dl copy
# in ~/sdk when the go on PATH is older.
want_go="go$(awk '/^go / {print $2}' go.mod)"
if [[ "$(go env GOVERSION 2>/dev/null | cut -d- -f1)" != "$want_go" && -x "$HOME/sdk/$want_go/bin/go" ]]; then
  export PATH="$HOME/sdk/$want_go/bin:$PATH"
fi
[[ "$(go env GOVERSION | cut -d- -f1)" == "$want_go" ]] || fail "$want_go is required to build the archives"

remote_tag_exists() { git ls-remote --exit-code --tags origin "refs/tags/$1" >/dev/null 2>&1; }

# wait_for_workflow FILE REF waits for the run FILE started for the tag REF
# and fails unless it succeeds.
wait_for_workflow() {
  local workflow="$1" ref="$2" run=""
  for _ in $(seq 60); do
    run="$(gh run list --repo "$REPO" --workflow "$workflow" --event push --limit 20 \
      --json databaseId,headBranch -q ".[] | select(.headBranch == \"$ref\") | .databaseId" | head -1)"
    [[ -n "$run" ]] && break
    sleep 10
  done
  [[ -n "$run" ]] || fail "no $workflow run started for $ref"
  gh run watch "$run" --repo "$REPO" --exit-status --interval 20 >/dev/null ||
    fail "$workflow failed for $ref: $(gh run view "$run" --repo "$REPO" --json url -q .url)"
}

git fetch --quiet --tags origin

# 1-2. Chart bump and release tag.
if remote_tag_exists "$VERSION"; then
  step "$VERSION is already pushed; skipping the chart bump and tag"
  git rev-parse --verify --quiet "refs/tags/$VERSION" >/dev/null || git fetch --quiet origin "refs/tags/$VERSION:refs/tags/$VERSION"
else
  [[ "$(git branch --show-current)" == "master" ]] || fail "check out master first"
  [[ -z "$(git status --porcelain)" ]] || fail "the working tree has uncommitted changes"
  [[ "$(git rev-parse HEAD)" == "$(git rev-parse origin/master)" ]] || fail "master differs from origin/master; pull or push first"

  chart_version="${CHART_VERSION:-}"
  if [[ -z "$chart_version" ]]; then
    current="$(awk '/^version:/ {print $2}' "$CHART")"
    chart_version="${current%.*}.$(( ${current##*.} + 1 ))"
  fi
  remote_tag_exists "chart-v$chart_version" && fail "chart-v$chart_version already exists; set CHART_VERSION"

  step "Bumping the chart to $chart_version (appVersion $APP_VERSION)"
  sed -i.bak -E "s/^version: .*/version: $chart_version/; s/^appVersion: .*/appVersion: \"$APP_VERSION\"/" "$CHART"
  rm -f "$CHART.bak"
  git --no-pager diff -- "$CHART"
  read -r -p "Commit this, then push master and the signed $VERSION tag? [y/N] " answer
  [[ "$answer" == [yY] ]] || { git checkout -- "$CHART"; fail "stopped before pushing anything"; }

  git commit --quiet -m "Release $VERSION" -- "$CHART"
  git tag -s "$VERSION" -m "$VERSION"
  git push --atomic origin master "$VERSION"
fi
TAG_COMMIT="$(git rev-list -n 1 "$VERSION")"
CHART_VERSION="$(git show "$TAG_COMMIT:$CHART" | awk '/^version:/ {print $2}')"
[[ "$(git show "$TAG_COMMIT:$CHART" | awk '/^appVersion:/ {print $2}' | tr -d '"')" == "$APP_VERSION" ]] ||
  fail "$CHART at $VERSION does not have appVersion $APP_VERSION"

# 3-4. Archives and draft release. The build runs in a worktree of the tag
# outside the repo, because it regenerates templ files and that disturbs a
# running `task dev`.
if gh release view "$VERSION" --repo "$REPO" --json assets -q '.assets[].name' 2>/dev/null | grep -q '^checksums.txt$'; then
  step "The $VERSION release already has its archives; skipping the build"
else
  step "Building the $VERSION archives"
  build="$(mktemp -d)"
  trap 'git -C "$ROOT" worktree remove --force "$build" >/dev/null 2>&1 || true' EXIT
  git worktree add --quiet --detach "$build" "$VERSION"
  (cd "$build" && VERSION="$VERSION" ./scripts/release-build.sh)

  if gh release view "$VERSION" --repo "$REPO" >/dev/null 2>&1; then
    step "Uploading the archives to the existing $VERSION release"
    gh release upload "$VERSION" "$build"/dist/* --repo "$REPO" --clobber
  else
    step "Creating the draft $VERSION release"
    gh release create "$VERSION" "$build"/dist/* --repo "$REPO" --draft --verify-tag --title "$VERSION" --notes-file "$NOTES"
  fi
fi

# 5. The chart defaults to the appVersion image, so it waits for the image.
step "Waiting for the Docker image build"
wait_for_workflow docker.yml "$VERSION"
image="${REPO,,}"
token="$(curl -fsS "https://ghcr.io/token?scope=repository:$image:pull" | sed -E 's/.*"token":"([^"]+)".*/\1/')"
curl -fsS -o /dev/null -H "Authorization: Bearer $token" \
  -H "Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json" \
  "https://ghcr.io/v2/$image/manifests/$APP_VERSION" ||
  fail "ghcr.io/$image:$APP_VERSION cannot be pulled"
echo "ghcr.io/$image:$APP_VERSION is available"

# 6. Chart tag.
if remote_tag_exists "chart-v$CHART_VERSION"; then
  step "chart-v$CHART_VERSION is already pushed"
else
  step "Pushing the signed chart-v$CHART_VERSION tag"
  git tag -s "chart-v$CHART_VERSION" -m "chart-v$CHART_VERSION" "$TAG_COMMIT"
  git push origin "chart-v$CHART_VERSION"
fi
step "Waiting for the chart to publish"
wait_for_workflow helm-release.yml "chart-v$CHART_VERSION"

# 7. Publish.
if [[ "$(gh release view "$VERSION" --repo "$REPO" --json isDraft -q .isDraft)" == "true" ]]; then
  step "Publishing the $VERSION release"
  gh release edit "$VERSION" --repo "$REPO" --draft=false --latest >/dev/null
fi
step "Released $VERSION: $(gh release view "$VERSION" --repo "$REPO" --json url -q .url)"
