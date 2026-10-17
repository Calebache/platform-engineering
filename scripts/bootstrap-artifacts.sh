#!/usr/bin/env bash
# Creates (or brings into line) the artifacts that must outlive an
# environment: the ECR repositories that hold the app images, and the S3
# bucket that holds the agent's audit records.
#
# They live outside Terraform for the same reason the state bucket does:
# `scripts/destroy-env.sh` tears the environment down completely, and these
# must survive it. Images, because the GitOps manifests pin exact tags and a
# rebuilt environment must be able to pull them; audit records, because an
# audit trail that disappears with the environment it audits is no audit
# trail. Terraform only looks them up (data sources) and fails fast if this
# script hasn't been run.
#
# Idempotent: re-running it re-applies every setting to existing resources.
#
# Usage:
#   scripts/bootstrap-artifacts.sh                 # dev
#   ENVIRONMENT=staging scripts/bootstrap-artifacts.sh
set -euo pipefail

ACCOUNT_ID="${ACCOUNT_ID:-103544553431}"
REGION="${REGION:-ca-central-1}"
PROJECT="${PROJECT:-agentic-sre}"
ENVIRONMENT="${ENVIRONMENT:-dev}"
REPOSITORIES="${REPOSITORIES:-mcp-server sre-agent payment-api}"
AUDIT_BUCKET="${AUDIT_BUCKET:-${PROJECT}-${ENVIRONMENT}-audit-${ACCOUNT_ID}}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

command -v aws >/dev/null || die "aws CLI not found"
caller_account="$(aws sts get-caller-identity --query Account --output text)" \
  || die "AWS credentials are not valid"
[[ "$caller_account" == "$ACCOUNT_ID" ]] \
  || die "credentials are for account $caller_account, expected $ACCOUNT_ID"

# --- ECR ------------------------------------------------------------------

lifecycle_policy='{"rules":[
  {"rulePriority":1,"description":"Expire untagged images after 7 days",
   "selection":{"tagStatus":"untagged","countType":"sinceImagePushed","countUnit":"days","countNumber":7},
   "action":{"type":"expire"}},
  {"rulePriority":2,"description":"Keep the 30 most recent images",
   "selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":30},
   "action":{"type":"expire"}}]}'

for repo in $REPOSITORIES; do
  if aws ecr describe-repositories --region "$REGION" --repository-names "$repo" >/dev/null 2>&1; then
    log "ECR $repo exists; re-applying settings"
  else
    log "Creating ECR repository $repo"
    aws ecr create-repository --region "$REGION" --repository-name "$repo" \
      --image-tag-mutability IMMUTABLE --encryption-configuration encryptionType=AES256 \
      --image-scanning-configuration scanOnPush=true >/dev/null
  fi
  # Tags are commit SHAs and can never be overwritten.
  aws ecr put-image-tag-mutability --region "$REGION" --repository-name "$repo" --image-tag-mutability IMMUTABLE >/dev/null
  aws ecr put-image-scanning-configuration --region "$REGION" --repository-name "$repo" --image-scanning-configuration scanOnPush=true >/dev/null
  aws ecr put-lifecycle-policy --region "$REGION" --repository-name "$repo" --lifecycle-policy-text "$lifecycle_policy" >/dev/null
done

# --- Audit bucket ---------------------------------------------------------

if aws s3api head-bucket --bucket "$AUDIT_BUCKET" --expected-bucket-owner "$ACCOUNT_ID" 2>/dev/null; then
  log "Audit bucket $AUDIT_BUCKET exists; re-applying settings"
else
  log "Creating audit bucket $AUDIT_BUCKET"
  create_args=(--bucket "$AUDIT_BUCKET" --region "$REGION" --object-ownership BucketOwnerEnforced)
  if [[ "$REGION" != "us-east-1" ]]; then
    create_args+=(--create-bucket-configuration "LocationConstraint=$REGION")
  fi
  aws s3api create-bucket "${create_args[@]}" >/dev/null \
    || die "could not create $AUDIT_BUCKET"
  aws s3api wait bucket-exists --bucket "$AUDIT_BUCKET" --expected-bucket-owner "$ACCOUNT_ID"
fi

aws s3api put-public-access-block --bucket "$AUDIT_BUCKET" \
  --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
aws s3api put-bucket-ownership-controls --bucket "$AUDIT_BUCKET" \
  --ownership-controls 'Rules=[{ObjectOwnership=BucketOwnerEnforced}]'
# Versioning keeps every phase of every incident record (the agent rewrites
# one key per incident) and anything overwritten by mistake.
aws s3api put-bucket-versioning --bucket "$AUDIT_BUCKET" --versioning-configuration Status=Enabled
aws s3api put-bucket-encryption --bucket "$AUDIT_BUCKET" \
  --server-side-encryption-configuration '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
aws s3api put-bucket-policy --bucket "$AUDIT_BUCKET" --policy "$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "DenyInsecureTransport",
      "Effect": "Deny",
      "Principal": "*",
      "Action": "s3:*",
      "Resource": ["arn:aws:s3:::${AUDIT_BUCKET}", "arn:aws:s3:::${AUDIT_BUCKET}/*"],
      "Condition": { "Bool": { "aws:SecureTransport": "false" } }
    }
  ]
}
EOF
)"

log "Done. ECR: $REPOSITORIES; audit bucket: $AUDIT_BUCKET"
