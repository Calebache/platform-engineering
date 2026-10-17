#!/usr/bin/env bash
# Brings an environment up from nothing and proves it works.
#
#   1. Preflight: tools, credentials, Terraform >= 1.11, the GitHub App
#      secrets (created once, outside Terraform), and every image the GitOps
#      manifests pin actually exists in ECR.
#   2. Bootstrap the long-lived pieces (idempotent): state bucket, ECR
#      repositories, audit bucket.
#   3. terraform init + apply: the AWS substrate and Argo CD.
#   4. Wait until every Argo CD Application is Synced and Healthy, the public
#      Gateway is Programmed and its certificate is Ready.
#   5. Prove the agent's path end to end: the MCP smoke test and whoami,
#      both as the sre-agent identity.
#
# Usage:
#   scripts/create-env.sh dev [extra terraform apply args, e.g. -auto-approve]
# Env: SKIP_MODEL_CHECK=true skips whoami's (billed) Bedrock call.
set -euo pipefail

ENVIRONMENT="${1:?usage: $0 <environment> [terraform apply args]}"
shift
PROJECT="${PROJECT:-agentic-sre}"
ACCOUNT_ID="${ACCOUNT_ID:-103544553431}"
REGION="${REGION:-ca-central-1}"
CLUSTER="${CLUSTER:-${PROJECT}-${ENVIRONMENT}}"
READY_TIMEOUT_SECONDS="${READY_TIMEOUT_SECONDS:-2700}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TF_DIR="$REPO_ROOT/terraform"
TFVARS="environments/${ENVIRONMENT}.tfvars"
BACKEND="environments/${ENVIRONMENT}.s3.tfbackend"
GITOPS="$REPO_ROOT/gitops/workloads/${ENVIRONMENT}"
REGISTRY="${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com"

log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# --- 1. Preflight -----------------------------------------------------------

for cmd in aws kubectl terraform python3; do
  command -v "$cmd" >/dev/null || die "$cmd not found"
done
[[ -f "$TF_DIR/$TFVARS" && -f "$TF_DIR/$BACKEND" ]] || die "missing terraform/$TFVARS or terraform/$BACKEND"

tf_version="$(terraform version -json | python3 -I -c 'import sys,json; print(json.load(sys.stdin)["terraform_version"])')"
python3 -I -c 'import sys; v=tuple(int(x) for x in sys.argv[1].split(".")[:2]); sys.exit(0 if v>=(1,11) else 1)' "$tf_version" \
  || die "Terraform $tf_version is too old; need >= 1.11 (brew install hashicorp/tap/terraform)"

caller_account="$(aws sts get-caller-identity --query Account --output text)" || die "AWS credentials are not valid"
[[ "$caller_account" == "$ACCOUNT_ID" ]] || die "credentials are for account $caller_account, expected $ACCOUNT_ID"

for secret in "agentic-sre/argocd/github-app" "${PROJECT}-${ENVIRONMENT}/mcp-server/github-app"; do
  aws secretsmanager describe-secret --region "$REGION" --secret-id "$secret" >/dev/null 2>&1 \
    || die "secret $secret is missing; create it with scripts/store-github-app.sh (see its header)"
done
log "Preflight: Terraform $tf_version, account $ACCOUNT_ID, GitHub App secrets present"

# Every image the manifests pin must exist, or workloads come up in
# ImagePullBackOff. (ECR outlives environments, so normally they do.)
missing=0
while read -r image; do
  repo_tag="${image#"$REGISTRY"/}"
  repo="${repo_tag%%:*}" tag="${repo_tag##*:}"
  if aws ecr describe-images --region "$REGION" --repository-name "$repo" --image-ids imageTag="$tag" >/dev/null 2>&1; then
    log "Image present: $repo:$tag"
  else
    printf 'error: image %s:%s is not in ECR (build it from that commit with scripts/build-push.sh)\n' "$repo" "$tag" >&2
    missing=1
  fi
done < <(grep -rhoE "${REGISTRY//./\\.}/[a-z0-9-]+:[A-Za-z0-9._-]+" "$GITOPS" | sort -u)
(( missing == 0 )) || die "missing images; not creating the environment"

# --- 2. Bootstrap (idempotent) ---------------------------------------------

log "Bootstrapping long-lived resources"
ACCOUNT_ID="$ACCOUNT_ID" REGION="$REGION" "$REPO_ROOT/scripts/bootstrap-state-bucket.sh"
ACCOUNT_ID="$ACCOUNT_ID" REGION="$REGION" PROJECT="$PROJECT" ENVIRONMENT="$ENVIRONMENT" "$REPO_ROOT/scripts/bootstrap-artifacts.sh"

# --- 3. Terraform -----------------------------------------------------------

log "terraform init ($ENVIRONMENT)"
terraform -chdir="$TF_DIR" init -backend-config="$BACKEND" -reconfigure -input=false >/dev/null
log "terraform apply ($ENVIRONMENT)"
terraform -chdir="$TF_DIR" apply -var-file="$TFVARS" "$@"

# --- 4. Wait for the platform ----------------------------------------------

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
export KUBECONFIG="$workdir/kubeconfig"
aws eks update-kubeconfig --region "$REGION" --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null

app_status() {
  kubectl -n argocd get applications -o json 2>/dev/null | python3 -I -c '
import sys, json
try:
    items = json.load(sys.stdin)["items"]
except Exception:
    sys.exit()
for a in sorted(items, key=lambda a: a["metadata"]["name"]):
    st = a.get("status", {})
    print(a["metadata"]["name"], st.get("sync", {}).get("status", "?") + "/" + st.get("health", {}).get("status", "?"))
'
}

expected_apps="$(ls "$REPO_ROOT/gitops/apps/$ENVIRONMENT" | grep -c '\.yaml$')" # includes project + repo secret
log "Waiting for every Argo CD Application to be Synced/Healthy (up to $((READY_TIMEOUT_SECONDS / 60)) min)"
deadline=$((SECONDS + READY_TIMEOUT_SECONDS)); last=""
while :; do
  status="$(app_status)"
  total="$(echo "$status" | grep -c . || true)"
  healthy="$(echo "$status" | grep -c ' Synced/Healthy$' || true)"
  line="$healthy/$total Applications Synced/Healthy"
  [[ "$line" != "$last" ]] && log "$line" && last="$line"
  # root + one per Application manifest (expected_apps minus the AppProject and repo Secret)
  if (( total >= expected_apps - 1 )) && (( healthy == total )); then
    break
  fi
  if (( SECONDS > deadline )); then
    echo "$status" | grep -v ' Synced/Healthy$' >&2
    die "timed out waiting for Argo CD"
  fi
  sleep 20
done

log "Waiting for the public Gateway and its certificate"
kubectl -n envoy-gateway-system wait gateway/public --for=condition=Programmed --timeout=600s >/dev/null
kubectl -n envoy-gateway-system wait certificate/wildcard-calebache-com-tls --for=condition=Ready --timeout=900s >/dev/null
log "Gateway Programmed; certificate Ready"

# --- 5. Prove the agent's path ---------------------------------------------

image_of() { grep -hoE "${REGISTRY//./\\.}/$1:[A-Za-z0-9._-]+" "$GITOPS/$2" | head -1; }
MCP_IMAGE="$(image_of mcp-server mcp-server/deployment.yaml)"
AGENT_IMAGE="$(image_of sre-agent sre-agent/orchestrator.yaml)"
MODEL_ID="$(grep -A1 'name: WORKER_MODEL_ID' "$GITOPS/sre-agent/orchestrator.yaml" | awk '/value:/ {print $2}')"
BEDROCK_REGION="$(grep -A1 'name: WORKER_BEDROCK_REGION' "$GITOPS/sre-agent/orchestrator.yaml" | awk '/value:/ {print $2}')"

run_as_agent() { # name image args-json env-json token-volume(true|false)
  local name="$1" image="$2" args="$3" env="$4" token="$5" mounts="" volumes=""
  if [[ "$token" == true ]]; then
    mounts=',"volumeMounts":[{"name":"t","mountPath":"/var/run/secrets/mcp","readOnly":true}]'
    volumes=',"volumes":[{"name":"t","projected":{"sources":[{"serviceAccountToken":{"audience":"mcp-server","expirationSeconds":600,"path":"token"}}]}}]'
  fi
  kubectl -n sre-agent run "$name-$RANDOM" --rm -i --restart=Never --quiet --image="$image" --overrides='{
    "spec":{"serviceAccountName":"sre-agent","automountServiceAccountToken":false,
     "securityContext":{"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}},
     "containers":[{"name":"c","image":"'"$image"'","args":'"$args"',"env":'"$env"',
       "securityContext":{"allowPrivilegeEscalation":false,"readOnlyRootFilesystem":true,"capabilities":{"drop":["ALL"]}}'"$mounts"'}]'"$volumes"'}}'
}

log "MCP smoke test as sre-agent"
run_as_agent mcp-smoke "$MCP_IMAGE" '["smoke","-service","payment-api"]' '[]' true

whoami_args='["whoami"]'
[[ "${SKIP_MODEL_CHECK:-false}" == true ]] && whoami_args='["whoami","--skip-model"]'
log "Identity check (whoami) as sre-agent"
run_as_agent whoami "$AGENT_IMAGE" "$whoami_args" '[{"name":"AWS_REGION","value":"'"$REGION"'"},{"name":"BEDROCK_REGION","value":"'"$BEDROCK_REGION"'"},{"name":"AUDIT_BUCKET","value":"'"${PROJECT}-${ENVIRONMENT}-audit-${ACCOUNT_ID}"'"},{"name":"MODEL_ID","value":"'"$MODEL_ID"'"}]' false

cat <<EOF

==> Environment $ENVIRONMENT is up and verified.
    kubectl:   aws eks update-kubeconfig --region $REGION --name $CLUSTER
    Expect:    payment-loadgen logs "error" for up to ~15 min while DNS
               negative caches expire (see CLAUDE.md); the alert pipeline
               itself is ready now.
EOF
