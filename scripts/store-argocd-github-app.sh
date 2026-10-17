#!/usr/bin/env bash
# Verifies Argo CD's read-only GitHub App and stores its credentials in AWS
# Secrets Manager. Argo CD uses them to mint installation tokens that expire
# after one hour, so no long-lived credential is ever sent to GitHub.
#
# Like the state bucket, the secret lives outside Terraform and outlives every
# environment. Terraform reads it as an ephemeral value and writes it into the
# cluster with a write-only attribute, so it never lands in state.
#
# One-time GitHub setup (UI, ~3 minutes):
#   1. GitHub -> Settings -> Developer settings -> GitHub Apps -> New GitHub App
#        - Name: e.g. calebache-argocd-readonly (must be globally unique)
#        - Homepage URL: https://github.com/Calebache/platform-engineering
#        - Webhook: untick "Active"
#        - Repository permissions: Contents = Read-only (Metadata = Read-only
#          is added automatically). Nothing else.
#        - Where can this app be installed: Only on this account
#   2. On the app page, note the App ID, then "Generate a private key" (.pem).
#   3. "Install App" -> Only select repositories -> platform-engineering.
#
# Usage:
#   APP_ID=123456 PEM_FILE=~/Downloads/app.private-key.pem scripts/store-argocd-github-app.sh
#
# Re-run with a new PEM_FILE to rotate the key, then bump
# argocd_repo_secret_revision in the tfvars and apply. Delete the .pem file
# from disk afterwards; Secrets Manager is the only copy that should remain.
set -euo pipefail

ACCOUNT_ID="${ACCOUNT_ID:-103544553431}"
REGION="${REGION:-ca-central-1}"
GITHUB_REPO="${GITHUB_REPO:-Calebache/platform-engineering}"
SECRET_NAME="${SECRET_NAME:-agentic-sre/argocd/github-app}"
APP_ID="${APP_ID:-}"
PEM_FILE="${PEM_FILE:-}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

for cmd in aws curl openssl python3; do
  command -v "$cmd" >/dev/null || die "$cmd not found"
done
[[ -n "$APP_ID" ]] || die "set APP_ID (shown on the GitHub App's settings page)"
[[ -n "$PEM_FILE" && -r "$PEM_FILE" ]] || die "set PEM_FILE to the app's downloaded private key"

caller_account="$(aws sts get-caller-identity --query Account --output text)" \
  || die "AWS credentials are not valid"
[[ "$caller_account" == "$ACCOUNT_ID" ]] \
  || die "credentials are for account $caller_account, expected $ACCOUNT_ID"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

# Short-lived (9 min) JWT that authenticates as the app itself.
now="$(date +%s)"
header="$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)"
payload="$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' "$((now - 60))" "$((now + 540))" "$APP_ID" | b64url)"
signature="$(printf '%s.%s' "$header" "$payload" | openssl dgst -sha256 -sign "$PEM_FILE" | b64url)"
jwt="${header}.${payload}.${signature}"

gh_api() {
  local method="$1" path="$2" token="$3"
  curl -sS -X "$method" \
    -H "Authorization: Bearer $token" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "https://api.github.com${path}"
}

json_field() { python3 -I -c 'import sys,json; d=json.load(sys.stdin); print(d.get(sys.argv[1]) or "")' "$1"; }

log "Looking up the app's installation on $GITHUB_REPO"
installation_id="$(gh_api GET "/repos/${GITHUB_REPO}/installation" "$jwt" | json_field id)"
[[ -n "$installation_id" ]] \
  || die "app $APP_ID is not installed on $GITHUB_REPO (or APP_ID/PEM_FILE don't match)"
log "Installation ID: $installation_id"

# Mint one installation token to prove the credentials work and to check the
# granted permissions are read-only, then revoke it immediately.
log "Checking the app's permissions"
gh_api POST "/app/installations/${installation_id}/access_tokens" "$jwt" >"$workdir/token.json"
probe_token="$(json_field token <"$workdir/token.json")"
[[ -n "$probe_token" ]] || die "could not mint an installation token: $(cat "$workdir/token.json")"
python3 -I - "$workdir/token.json" <<'EOF' || { gh_api DELETE /installation/token "$probe_token" >/dev/null; exit 1; }
import json, sys
perms = json.load(open(sys.argv[1])).get("permissions", {})
allowed = {"contents": "read", "metadata": "read"}
extra = {k: v for k, v in perms.items() if allowed.get(k) != v}
if perms.get("contents") != "read":
    sys.exit("error: app needs Contents = Read-only (got %r)" % perms.get("contents"))
if extra:
    sys.exit("error: app has more than read-only contents access: %s. Remove these permissions." % extra)
print("==> Permissions OK:", perms)
EOF
gh_api DELETE /installation/token "$probe_token" >/dev/null || true

log "Writing credentials to $SECRET_NAME"
python3 -I - "$APP_ID" "$installation_id" "$PEM_FILE" "$workdir/secret.json" <<'EOF'
import json, sys
app_id, installation_id, pem_file, out = sys.argv[1:5]
with open(pem_file) as f:
    key = f.read()
with open(out, "w") as f:
    json.dump({"appId": app_id, "installationId": installation_id, "privateKey": key}, f)
EOF

if aws secretsmanager describe-secret --region "$REGION" --secret-id "$SECRET_NAME" >/dev/null 2>&1; then
  aws secretsmanager put-secret-value --region "$REGION" \
    --secret-id "$SECRET_NAME" \
    --secret-string "file://$workdir/secret.json" >/dev/null
  log "Stored a new version. Bump argocd_repo_secret_revision in the tfvars and apply."
else
  aws secretsmanager create-secret --region "$REGION" \
    --name "$SECRET_NAME" \
    --description "Argo CD read-only GitHub App credentials (${GITHUB_REPO})" \
    --secret-string "file://$workdir/secret.json" >/dev/null
  log "Created secret."
fi

log "Done. Now delete $PEM_FILE from disk."
