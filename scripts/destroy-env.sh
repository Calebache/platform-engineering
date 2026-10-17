#!/usr/bin/env bash
# Tears an environment down completely, in the order AWS and Argo CD need.
#
# Argo CD creates AWS resources Terraform doesn't know about: the NLB (via
# the AWS Load Balancer Controller) and Route53 records (via external-dns).
# They must be removed while those controllers are still running, or the
# NLB's ENIs block VPC deletion and DNS records are orphaned. And every Argo
# CD Application must finish deleting before Argo CD itself is uninstalled,
# or its finalizers are never processed and the argocd namespace hangs.
#
#   1. Pause the root app so nothing is re-created.
#   2. Delete child Applications wave by wave, highest first, waiting for
#      each wave to be fully gone before the next. Workloads and the Gateway
#      (waves >= 1) go before the controllers that clean up after them.
#   3. After the Gateway's wave: wait until no LoadBalancer Services or
#      HTTPRoutes remain, and give external-dns time to delete its records.
#   4. Delete the root Application itself and wait for it.
#   5. terraform destroy: Argo CD, EKS, the network, IAM.
#
# What survives (by design): the Terraform state bucket, ECR images, the CI
# roles and the audit bucket (scripts/bootstrap-*.sh), and the GitHub App secrets.
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
# The Gateway's wave: once it and everything above it are gone, the NLB and
# DNS records must be gone too, before lower-wave controllers are removed.
GATEWAY_WAVE="${GATEWAY_WAVE:-1}"
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

# Child Applications as "wave name" lines, highest wave first.
child_apps_by_wave() {
  kubectl -n argocd get applications -o json | python3 -I -c '
import sys, json
apps = []
for a in json.load(sys.stdin)["items"]:
    name = a["metadata"]["name"]
    if name == "root":
        continue
    wave = int((a["metadata"].get("annotations") or {}).get("argocd.argoproj.io/sync-wave", "0"))
    apps.append((wave, name))
for wave, name in sorted(apps, reverse=True):
    print(wave, name)
'
}

if aws eks describe-cluster --region "$REGION" --name "$CLUSTER" >/dev/null 2>&1; then
  # Temporary kubeconfig: the user's own ~/.kube/config is left untouched.
  aws eks update-kubeconfig --region "$REGION" --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null

  if kubectl -n argocd get application root >/dev/null 2>&1; then
    log "Pausing auto-sync on the root app"
    kubectl -n argocd patch application root --type merge -p '{"spec":{"syncPolicy":{"automated":null}}}' >/dev/null

    listing="$(child_apps_by_wave)"
    waves="$(echo "$listing" | awk 'NF {print $1}' | uniq)"
    gateway_cleanup_done=false
    for wave in $waves; do
      # Before the first wave below the Gateway's, the NLB and DNS records
      # must be gone: the controllers that remove them are about to go.
      if ! $gateway_cleanup_done && (( wave < GATEWAY_WAVE )); then
        log "Waiting for LoadBalancer Services to be gone (NLB deletion)"
        wait_for "LoadBalancer Services to be deleted" '[[ "$(count_lb_services)" == 0 ]]'
        log "Waiting for HTTPRoutes to be gone"
        wait_for "HTTPRoutes to be deleted" '[[ "$(count_httproutes)" == 0 ]]'
        log "Giving external-dns two sync intervals to remove its records"
        sleep 120
        gateway_cleanup_done=true
      fi
      apps="$(echo "$listing" | awk -v w="$wave" '$1 == w {print $2}')"
      for app in $apps; do
        log "Wave $wave: deleting Application $app (cascades to its resources)"
        kubectl -n argocd delete application "$app" --wait=false >/dev/null
      done
      for app in $apps; do
        wait_for "Application $app to be deleted" \
          "! kubectl -n argocd get application '$app' >/dev/null 2>&1"
      done
    done

    log "Deleting the root Application (AppProject and repo Secret go with it)"
    kubectl -n argocd delete application root --wait=false >/dev/null
    wait_for "the root Application to be deleted" \
      "! kubectl -n argocd get application root >/dev/null 2>&1"
  fi

  # Belt and braces: nothing Argo created may still hold AWS resources.
  [[ "$(count_lb_services)" == 0 ]] || die "LoadBalancer Services still exist; not running terraform destroy"
else
  log "Cluster $CLUSTER not found; skipping Kubernetes cleanup"
fi

cd "$REPO_ROOT/terraform"
terraform init -backend-config="environments/${ENVIRONMENT}.s3.tfbackend" -reconfigure -input=false >/dev/null

# ECR repositories, the audit bucket and the CI roles must survive. Terraform
# stops managing them through `removed` blocks, but those only take effect on
# an apply; until one has run they're still in state, and destroy would
# delete them. Forget them explicitly (this deletes nothing in AWS).
legacy="$(terraform state list 2>/dev/null | grep -E '^module\.(ecr|audit_bucket|github_oidc)\.' || true)"
if [[ -n "$legacy" ]]; then
  log "Forgetting long-lived artifacts still in state (not deleting them):"
  echo "$legacy" | sed 's/^/      /'
  # shellcheck disable=SC2086
  terraform state rm $legacy >/dev/null
fi

# Refuse to continue if the destroy would touch anything long-lived.
if terraform plan -destroy -var-file="$TFVARS" -input=false -no-color 2>/dev/null \
    | grep -E '# .*(aws_ecr_repository|aws_s3_bucket|aws_iam_openid_connect_provider)\..* will be destroyed'; then
  die "the destroy plan includes ECR repositories, S3 buckets or the GitHub OIDC provider; refusing (they must outlive the environment)"
fi

log "Running terraform destroy for $ENVIRONMENT"
terraform destroy -var-file="$TFVARS" "$@"
log "Environment $ENVIRONMENT destroyed. Kept: state bucket, ECR images, audit records, GitHub App secrets, CI roles."
