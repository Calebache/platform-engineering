#!/usr/bin/env bash
# Creates (or brings into line) everything GitHub Actions needs to reach AWS:
# the account's GitHub OIDC provider and one IAM role per CI duty. Run it once
# per account (and per environment name), before the first CI run.
#
# Like the state bucket and ECR, these live outside Terraform: CI must be
# able to authenticate while the environment is torn down (to build images,
# or to bring the environment back up), and the deployer role can't be
# created by the very apply it runs.
#
# Roles (each trusts exactly one workflow file on one ref, no static keys):
#   <project>-<env>-terraform-deployer-role  environment.yml on main   AdministratorAccess
#   <project>-<env>-terraform-plan-role      ci.yml on pull requests   ReadOnlyAccess + state lock + Argo CD's app secret
#   <project>-<env>-image-builder-role       images.yml on main        push to the platform's ECR repos
#
# Trust is pinned by the OIDC `sub` claim. This script sets the repository's
# sub template to include the workflow file (`job_workflow_ref`), so a job in
# one workflow can't assume another workflow's role. The repo uses GitHub's
# immutable subject format, so the claim carries the owner and repository IDs
# (`repo:Owner@ID/name@ID:…`) and survives renames, but never matches a
# different repository that later takes the same name.
#
# It also allows GitHub Actions to open pull requests (images.yml opens the
# image-pin PR). Both GitHub settings need repo admin, so `gh` must be logged
# in as the repository owner.
#
# Idempotent: re-running it re-applies every trust and permission policy.
#
# Usage:
#   scripts/bootstrap-ci.sh                 # dev
#   ENVIRONMENT=staging scripts/bootstrap-ci.sh
set -euo pipefail

ACCOUNT_ID="${ACCOUNT_ID:-103544553431}"
REGION="${REGION:-ca-central-1}"
PROJECT="${PROJECT:-agentic-sre}"
ENVIRONMENT="${ENVIRONMENT:-dev}"
GITHUB_REPO="${GITHUB_REPO:-Calebache/platform-engineering}"
STATE_BUCKET="${STATE_BUCKET:-calebache-platform-tfstate-${ACCOUNT_ID}}"
REPOSITORIES="${REPOSITORIES:-mcp-server sre-agent payment-api}"
# environment.yml runs create-env.sh/destroy-env.sh end to end (up to ~1 h).
DEPLOYER_SESSION_SECONDS="${DEPLOYER_SESSION_SECONDS:-10800}"

OIDC_HOST="token.actions.githubusercontent.com"
OIDC_ARN="arn:aws:iam::${ACCOUNT_ID}:oidc-provider/${OIDC_HOST}"
NAME="${PROJECT}-${ENVIRONMENT}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

for cmd in aws gh python3; do
  command -v "$cmd" >/dev/null || die "$cmd not found"
done
caller_account="$(aws sts get-caller-identity --query Account --output text)" \
  || die "AWS credentials are not valid"
[[ "$caller_account" == "$ACCOUNT_ID" ]] \
  || die "credentials are for account $caller_account, expected $ACCOUNT_ID"

# --- GitHub: subject template and PR permission ---------------------------

log "Setting $GITHUB_REPO's OIDC subject template to repo + context + job_workflow_ref"
gh api -X PUT "repos/$GITHUB_REPO/actions/oidc/customization/sub" --input - >/dev/null <<'JSON'
{"use_default": false, "use_immutable_subject": true, "include_claim_keys": ["repo", "context", "job_workflow_ref"]}
JSON
template="$(gh api "repos/$GITHUB_REPO/actions/oidc/customization/sub")"
python3 -I -c '
import sys, json
t = json.load(sys.stdin)
ok = not t.get("use_default") and t.get("use_immutable_subject") and t.get("include_claim_keys") == ["repo", "context", "job_workflow_ref"]
sys.exit(0 if ok else 1)' <<<"$template" || die "subject template didn't stick: $template"
# The immutable `repo` segment: names plus the owner's and repository's IDs.
prefix="$(gh api "repos/$GITHUB_REPO" --jq '"repo:\(.owner.login)@\(.owner.id)/\(.name)@\(.id)"')"
log "Subject prefix: $prefix"

log "Allowing GitHub Actions to open pull requests (workflows default to read-only tokens)"
gh api -X PUT "repos/$GITHUB_REPO/actions/permissions/workflow" \
  -f default_workflow_permissions=read -F can_approve_pull_request_reviews=true >/dev/null

# --- OIDC provider (one per account) --------------------------------------

if aws iam get-open-id-connect-provider --open-id-connect-provider-arn "$OIDC_ARN" >/dev/null 2>&1; then
  log "OIDC provider exists"
else
  log "Creating the GitHub OIDC provider"
  # AWS validates GitHub's certificate itself; the thumbprint is unused.
  aws iam create-open-id-connect-provider --url "https://${OIDC_HOST}" \
    --client-id-list sts.amazonaws.com \
    --thumbprint-list 6938fd4d98bab03faadb97b34396831e3780aea1 >/dev/null
fi

# --- Roles ----------------------------------------------------------------

# trust_policy <sub pattern>: StringLike, because job_workflow_ref repeats the
# repository name and, on pull requests, carries the PR number.
trust_policy() {
  python3 -I -c '
import json, sys
arn, host, sub = sys.argv[1:]
print(json.dumps({"Version": "2012-10-17", "Statement": [{
  "Effect": "Allow",
  "Principal": {"Federated": arn},
  "Action": "sts:AssumeRoleWithWebIdentity",
  "Condition": {
    "StringEquals": {host + ":aud": "sts.amazonaws.com"},
    "StringLike": {host + ":sub": sub},
  }}]}))' "$OIDC_ARN" "$OIDC_HOST" "$1"
}

# ensure_role <name> <sub pattern> <max session seconds>
ensure_role() {
  local role="$1" trust
  trust="$(trust_policy "$2")"
  if aws iam get-role --role-name "$role" >/dev/null 2>&1; then
    log "Role $role exists; re-applying trust policy"
    aws iam update-assume-role-policy --role-name "$role" --policy-document "$trust"
    aws iam update-role --role-name "$role" --max-session-duration "$3"
  else
    log "Creating role $role"
    aws iam create-role --role-name "$role" --assume-role-policy-document "$trust" \
      --max-session-duration "$3" --tags Key=Project,Value="$PROJECT" Key=Environment,Value="$ENVIRONMENT" \
      Key=ManagedBy,Value=scripts/bootstrap-ci.sh >/dev/null
  fi
  log "  trusts sub like: $2"
}

workflow_sub() { # workflow_sub <context> <workflow file> <ref glob>
  printf '%s:%s:job_workflow_ref:*/.github/workflows/%s@%s' "$prefix" "$1" "$2" "$3"
}

# terraform apply/destroy, via create-env.sh and destroy-env.sh. Effectively
# admin (it creates IAM, EKS and VPC resources); contained by its trust policy
# (one workflow file, only on main, only dispatched by the owner) and never
# usable by workloads.
deployer="${NAME}-terraform-deployer-role"
ensure_role "$deployer" "$(workflow_sub ref:refs/heads/main environment.yml refs/heads/main)" "$DEPLOYER_SESSION_SECONDS"
aws iam attach-role-policy --role-name "$deployer" --policy-arn arn:aws:iam::aws:policy/AdministratorAccess

# terraform plan on pull requests: read-only, plus the state lock, plus the
# Argo CD GitHub App secret the plan reads ephemerally (a read-only app).
plan="${NAME}-terraform-plan-role"
ensure_role "$plan" "$(workflow_sub pull_request ci.yml 'refs/pull/*')" 3600
aws iam attach-role-policy --role-name "$plan" --policy-arn arn:aws:iam::aws:policy/ReadOnlyAccess
aws iam put-role-policy --role-name "$plan" --policy-name plan-extras --policy-document "$(python3 -I -c '
import json, sys
bucket, region, account = sys.argv[1:]
print(json.dumps({"Version": "2012-10-17", "Statement": [
  {"Sid": "StateLock", "Effect": "Allow", "Action": ["s3:PutObject", "s3:DeleteObject"],
   "Resource": f"arn:aws:s3:::{bucket}/*.tflock"},
  {"Sid": "ArgoCdRepoCredential", "Effect": "Allow", "Action": "secretsmanager:GetSecretValue",
   "Resource": f"arn:aws:secretsmanager:{region}:{account}:secret:agentic-sre/argocd/github-app-*"},
]}))' "$STATE_BUCKET" "$REGION" "$ACCOUNT_ID")"

# Image pushes, only from main, only to the platform's repositories.
builder="${NAME}-image-builder-role"
ensure_role "$builder" "$(workflow_sub ref:refs/heads/main images.yml refs/heads/main)" 3600
# shellcheck disable=SC2086
aws iam put-role-policy --role-name "$builder" --policy-name ecr-push --policy-document "$(python3 -I -c '
import json, sys
region, account, *repos = sys.argv[1:]
print(json.dumps({"Version": "2012-10-17", "Statement": [
  {"Sid": "EcrLogin", "Effect": "Allow", "Action": "ecr:GetAuthorizationToken", "Resource": "*"},
  {"Sid": "PushToPlatformRepos", "Effect": "Allow",
   "Action": ["ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:CompleteLayerUpload",
              "ecr:DescribeImageScanFindings", "ecr:DescribeImages", "ecr:InitiateLayerUpload",
              "ecr:PutImage", "ecr:UploadLayerPart"],
   "Resource": [f"arn:aws:ecr:{region}:{account}:repository/{r}" for r in repos]},
]}))' "$REGION" "$ACCOUNT_ID" $REPOSITORIES)"

log "Done. Workflows assume arn:aws:iam::${ACCOUNT_ID}:role/<role> for the roles above."
