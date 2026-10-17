#!/usr/bin/env bash
# Pins an app's image to a tag in an environment's GitOps manifests: every
# reference to <registry>/<app>:<anything> under gitops/workloads/<env>
# becomes <registry>/<app>:<tag>. CI's image workflow uses it to open the pin
# PR; run it by hand after scripts/build-push.sh.
#
# Usage: scripts/pin-image.sh <app> <tag> [environment]
set -euo pipefail

APP="${1:?usage: $0 <app> <tag> [environment]}"
TAG="${2:?usage: $0 <app> <tag> [environment]}"
ENVIRONMENT="${3:-dev}"
ACCOUNT_ID="${ACCOUNT_ID:-103544553431}"
REGION="${REGION:-ca-central-1}"
REGISTRY="${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIR="$REPO_ROOT/gitops/workloads/$ENVIRONMENT"

[[ "$APP" =~ ^[a-z0-9-]+$ && "$TAG" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "invalid app or tag" >&2; exit 1; }
pattern="${REGISTRY//./\\.}/${APP}:[A-Za-z0-9._-]+"
files="$(grep -rlE "$pattern" "$DIR" || true)"
[[ -n "$files" ]] || { echo "no manifest in $DIR references $REGISTRY/$APP" >&2; exit 1; }
for f in $files; do
  # Portable in-place edit (BSD and GNU sed differ on -i).
  sed -E "s#${pattern}#${REGISTRY}/${APP}:${TAG}#g" "$f" >"$f.tmp" && mv "$f.tmp" "$f"
  echo "pinned $APP:$TAG in ${f#"$REPO_ROOT"/}"
done
