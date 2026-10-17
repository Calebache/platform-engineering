#!/usr/bin/env bash
# Builds an app image for the cluster's architecture and pushes it to ECR,
# tagged with the Git commit SHA (ECR tags are immutable; never `latest`).
#
# Stopgap until GitHub Actions builds images with the image-builder OIDC role.
#
# Usage:
#   scripts/build-push.sh payment-api        # builds apps/payment-api
# Prints the pushed image reference on the last line.
set -euo pipefail

APP="${1:?usage: $0 <app>}"
ACCOUNT_ID="${ACCOUNT_ID:-103544553431}"
REGION="${REGION:-ca-central-1}"
PLATFORM="${PLATFORM:-linux/amd64}"
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

log "Logging in to $REGISTRY"
aws ecr get-login-password --region "$REGION" | docker login --username AWS --password-stdin "$REGISTRY" >/dev/null

log "Building $IMAGE for $PLATFORM"
docker buildx build --platform "$PLATFORM" --build-arg VERSION="$TAG" \
  --provenance=false --tag "$IMAGE" --push "$APP_DIR"

if command -v trivy >/dev/null; then
  log "Scanning with Trivy (fails on HIGH/CRITICAL)"
  trivy image --quiet --exit-code 1 --severity HIGH,CRITICAL "$IMAGE"
else
  log "trivy not installed; relying on ECR scan-on-push"
fi

echo "$IMAGE"
