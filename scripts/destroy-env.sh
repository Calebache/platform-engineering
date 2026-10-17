#!/usr/bin/env bash
# Tears down an environment in the order AWS needs.
#
# Argo CD creates AWS resources Terraform doesn't know about: the NLB (via the
# AWS Load Balancer Controller) and Route53 records (via external-dns). They
# must be removed while those controllers are still running, otherwise the
# NLB's ENIs block VPC deletion and DNS records are orphaned. So:
#   1. stop the root app from re-creating anything,
#   2. delete every Application from wave 0 upwards (Gateway, routes, apps),
#   3. wait until no LoadBalancer Services or HTTPRoutes remain,
#   4. run terraform destroy, which removes the controllers and everything else.
#
# Usage:
#   scripts/destroy-env.sh dev [extra terraform destroy args, e.g. -auto-approve]
set -euo pipefail

ENVIRONMENT="${1:?usage: $0 <environment> [terraform destroy args]}"
shift
PROJECT="${PROJECT:-agentic-sre}"
REGION="${REGION:-ca-central-1}"
CLUSTER="${CLUSTER:-${PROJECT}-${ENVIRONMENT}}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-900}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TFVARS="environments/${ENVIRONMENT}.tfvars"

log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

for cmd in aws kubectl terraform python3; do
  command -v "$cmd" >/dev/null || die "$cmd not found"
done
[[ -f "$REPO_ROOT/terraform/$TFVARS" ]] || die "no terraform/$TFVARS"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
export KUBECONFIG="$workdir/kubeconfig"

wait_for() {
  local description="$1" check="$2" deadline=$((SECONDS + TIMEOUT_SECONDS))
  until eval "$check"; do
    (( SECONDS < deadline )) || die "timed out waiting for: $description"
    sleep 10
  done
}

count_lb_services() {
  kubectl get svc -A -o json | python3 -I -c \
    'import sys,json; print(sum(1 for s in json.load(sys.stdin)["items"] if s["spec"].get("type")=="LoadBalancer"))'
}

count_httproutes() {
  kubectl get httproutes.gateway.networking.k8s.io -A --no-headers 2>/dev/null | wc -l | tr -d ' '
}

if aws eks describe-cluster --region "$REGION" --name "$CLUSTER" >/dev/null 2>&1; then
  # Temporary kubeconfig: the user's own ~/.kube/config is left untouched.
  aws eks update-kubeconfig --region "$REGION" --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null

  if kubectl -n argocd get application root >/dev/null 2>&1; then
    log "Pausing auto-sync on the root app"
    kubectl -n argocd patch application root --type merge -p '{"spec":{"syncPolicy":{"automated":null}}}' >/dev/null

    # Wave >= 0 is config and workloads (Gateway, routes, apps); wave < 0 is
    # the controllers that must outlive them.
    apps="$(kubectl -n argocd get applications -o json | python3 -I -c '
import sys, json
for a in json.load(sys.stdin)["items"]:
    name = a["metadata"]["name"]
    wave = int((a["metadata"].get("annotations") or {}).get("argocd.argoproj.io/sync-wave", "0"))
    if name != "root" and wave >= 0:
        print(name)
')"
    for app in $apps; do
      log "Deleting Application $app (cascades to its resources)"
      kubectl -n argocd delete application "$app" --wait=false >/dev/null
    done
    for app in $apps; do
      wait_for "Application $app to be deleted" \
        "! kubectl -n argocd get application '$app' >/dev/null 2>&1"
    done
  fi

  log "Waiting for LoadBalancer Services to be gone (NLB deletion)"
  wait_for "LoadBalancer Services to be deleted" '[[ "$(count_lb_services)" == 0 ]]'

  log "Waiting for HTTPRoutes to be gone"
  wait_for "HTTPRoutes to be deleted" '[[ "$(count_httproutes)" == 0 ]]'

  log "Giving external-dns two sync intervals to remove its records"
  sleep 120
else
  log "Cluster $CLUSTER not found; skipping Kubernetes cleanup"
fi

log "Running terraform destroy for $ENVIRONMENT"
cd "$REPO_ROOT/terraform"
terraform destroy -var-file="$TFVARS" "$@"
