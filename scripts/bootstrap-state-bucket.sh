#!/usr/bin/env bash
# Creates (or brings into line) the S3 bucket that holds Terraform state.
#
# Terraform deliberately does not manage its own state bucket: that would need
# local state for the bucket and a chicken-and-egg migration. This script is
# idempotent, so re-running it re-applies the settings to an existing bucket.
#
# Usage:
#   scripts/bootstrap-state-bucket.sh
#   ACCOUNT_ID=... REGION=... BUCKET=... scripts/bootstrap-state-bucket.sh
set -euo pipefail

ACCOUNT_ID="${ACCOUNT_ID:-103544553431}"
REGION="${REGION:-ca-central-1}"
BUCKET="${BUCKET:-calebache-platform-tfstate-${ACCOUNT_ID}}"
NONCURRENT_VERSION_DAYS="${NONCURRENT_VERSION_DAYS:-90}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

command -v aws >/dev/null || die "aws CLI not found"

caller_account="$(aws sts get-caller-identity --query Account --output text)" \
  || die "AWS credentials are not valid"
[[ "$caller_account" == "$ACCOUNT_ID" ]] \
  || die "credentials are for account $caller_account, expected $ACCOUNT_ID"

if aws s3api head-bucket --bucket "$BUCKET" --expected-bucket-owner "$ACCOUNT_ID" 2>/dev/null; then
  log "Bucket $BUCKET already exists; re-applying settings"
else
  log "Creating bucket $BUCKET in $REGION"
  create_args=(--bucket "$BUCKET" --region "$REGION" --object-ownership BucketOwnerEnforced)
  # us-east-1 rejects an explicit LocationConstraint.
  if [[ "$REGION" != "us-east-1" ]]; then
    create_args+=(--create-bucket-configuration "LocationConstraint=$REGION")
  fi
  aws s3api create-bucket "${create_args[@]}" >/dev/null \
    || die "could not create $BUCKET (the name may be taken by another account)"
  aws s3api wait bucket-exists --bucket "$BUCKET" --expected-bucket-owner "$ACCOUNT_ID"
fi

log "Blocking public access"
aws s3api put-public-access-block --bucket "$BUCKET" \
  --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true

log "Enforcing bucket-owner object ownership"
aws s3api put-bucket-ownership-controls --bucket "$BUCKET" \
  --ownership-controls 'Rules=[{ObjectOwnership=BucketOwnerEnforced}]'

log "Enabling versioning"
aws s3api put-bucket-versioning --bucket "$BUCKET" \
  --versioning-configuration Status=Enabled

log "Enabling default encryption (SSE-S3)"
aws s3api put-bucket-encryption --bucket "$BUCKET" \
  --server-side-encryption-configuration \
  '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'

log "Expiring noncurrent state versions after ${NONCURRENT_VERSION_DAYS} days"
aws s3api put-bucket-lifecycle-configuration --bucket "$BUCKET" \
  --lifecycle-configuration "$(cat <<EOF
{
  "Rules": [
    {
      "ID": "expire-old-state-versions",
      "Status": "Enabled",
      "Filter": {},
      "NoncurrentVersionExpiration": { "NoncurrentDays": ${NONCURRENT_VERSION_DAYS} },
      "AbortIncompleteMultipartUpload": { "DaysAfterInitiation": 7 }
    }
  ]
}
EOF
)"

log "Denying non-TLS requests"
aws s3api put-bucket-policy --bucket "$BUCKET" --policy "$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "DenyInsecureTransport",
      "Effect": "Deny",
      "Principal": "*",
      "Action": "s3:*",
      "Resource": ["arn:aws:s3:::${BUCKET}", "arn:aws:s3:::${BUCKET}/*"],
      "Condition": { "Bool": { "aws:SecureTransport": "false" } }
    }
  ]
}
EOF
)"

log "Done. State bucket: $BUCKET"
