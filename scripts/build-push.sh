#!/usr/bin/env bash
# Builds an app image for the cluster's architecture and pushes it to ECR,
# tagged with the Git commit SHA (ECR tags are immutable; never `latest`).
#
# CI (.github/workflows/images.yml) runs it on every merge to main that
# changes an app; you can run it by hand too. The image is scanned before
# it's pushed: a tag can't be overwritten, so a vulnerable image must never
# reach ECR.
#
# Usage:
#   scripts/build-push.sh payment-api        # builds apps/payment-api
# Env: TRIVY overrides the scanner command (CI runs it in Docker);
#      REQUIRE_SCAN=true fails if no scanner is available.
# stdout is only the pushed image reference; everything else goes to stderr.
set -euo pipefail

APP="${1:?usage: $0 <app>}"
ACCOUNT_ID="${ACCOUNT_ID:-103544553431}"
REGION="${REGION:-ca-central-1}"
PLATFORM="${PLATFORM:-linux/amd64}"
TRIVY="${TRIVY:-trivy}"
REQUIRE_SCAN="${REQUIRE_SCAN:-false}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APP_DIR="$REPO_ROOT/apps/$APP"
REGISTRY="${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[[ -d "$APP_DIR" ]] || die "no app at apps/$APP"
for cmd in aws docker git; do command -v "$cmd" >/dev/null || die "$cmd not found"; done

# The tag must identify exactly what was built, so the app's directory must
# be committed.
if [[ -n "$(git -C "$REPO_ROOT" status --porcelain -- "apps/$APP")" ]]; then
  die "apps/$APP has uncommitted changes; commit first so the tag matches the source"
fi
TAG="$(git -C "$REPO_ROOT" rev-parse --short=12 HEAD)"
IMAGE="${REGISTRY}/${APP}:${TAG}"

if aws ecr describe-images --region "$REGION" --repository-name "$APP" --image-ids imageTag="$TAG" >/dev/null 2>&1; then
  log "$IMAGE already exists (immutable tag); nothing to do"
  echo "$IMAGE"
  exit 0
fi

log "Building $IMAGE for $PLATFORM"
docker buildx build --platform "$PLATFORM" --build-arg VERSION="$TAG" \
  --provenance=false --tag "$IMAGE" --load "$APP_DIR" >&2

# shellcheck disable=SC2086 # TRIVY may be a multi-word command
if command -v ${TRIVY%% *} >/dev/null; then
  log "Scanning with Trivy (fails on fixable HIGH/CRITICAL)"
  $TRIVY image --quiet --exit-code 1 --severity HIGH,CRITICAL --ignore-unfixed "$IMAGE" >&2
elif [[ "$REQUIRE_SCAN" == true ]]; then
  die "trivy not available and REQUIRE_SCAN=true"
else
  log "trivy not installed; relying on ECR scan-on-push"
fi

log "Logging in to $REGISTRY"
aws ecr get-login-password --region "$REGION" | docker login --username AWS --password-stdin "$REGISTRY" >/dev/null
log "Pushing $IMAGE"
docker push --quiet "$IMAGE" >&2
# Callers (CI's pin PR) trust this reference, so prove the tag is really there.
aws ecr describe-images --region "$REGION" --repository-name "$APP" --image-ids imageTag="$TAG" >/dev/null \
  || die "pushed $IMAGE but ECR doesn't list it"

echo "$IMAGE"
