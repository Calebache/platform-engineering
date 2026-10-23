# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Current state

[doc.md](doc.md) is the implementation summary for an **Agentic SRE Platform PoC** on AWS; read it before building anything. So far there's the Terraform foundation, the Argo CD bootstrap (one `terraform apply` does both), the platform Applications (Envoy Gateway, AWS Load Balancer Controller, cluster-autoscaler, cert-manager, external-dns, plus the public Gateway), observability (kube-prometheus-stack), External Secrets, the `payment-api` demo workload, the MCP server, the SRE agent (one Kubernetes Job per incident), and CI/CD (GitHub Actions: PR validation, image builds with pin PRs, and environment up/down). Update this file as things land.

## Layout and commands

Terraform follows a **root module + one module per resource type + per-environment tfvars** layout:

- `terraform/` (root): `main.tf` only wires modules together. `policies.tf` holds the IAM permission policies passed into modules. `variables.tf` leaves environment-specific values without defaults, so they must come from tfvars. The backend is a partial `backend "s3" {}`.
- `terraform/modules/<type>/`: `network`, `eks`, `pod-identity-role`, `argocd` and `app-secret`. Each has `main.tf`, `variables.tf`, `outputs.tf` and `versions.tf`. Put new resource types in a new module, not in the root. `pod-identity-role` binds one IAM role to one namespace/ServiceAccount; add a new workload identity as an entry in `local.workload_identities` in `main.tf`.
- `terraform/environments/<env>.tfvars` sets an environment's values. `<env>.s3.tfbackend` sets its state key. To add an environment, copy both and change the values. Run `scripts/bootstrap-ci.sh` with `ENVIRONMENT=<env>` for its CI roles. ECR repo names must not overlap within an account.
- `scripts/bootstrap-state-bucket.sh` creates the S3 state bucket. Terraform deliberately **doesn't manage its own state bucket**, which avoids local state and the chicken-and-egg migration. The script is idempotent: re-running it re-applies versioning, encryption, public-access block, lifecycle and the TLS-only policy. Override with the `ACCOUNT_ID`, `REGION` and `BUCKET` env vars. Run it once per account before the first `terraform init`. The bucket outlives every environment.
- `scripts/bootstrap-artifacts.sh <env>` creates or adopts the **ECR repos and the audit bucket**, which also live outside Terraform so a teardown keeps images and audit records. It's idempotent and re-applies immutable tags, scan-on-push, the lifecycle policy, versioning, encryption and the TLS-only policy. Terraform only reads them (`data.aws_ecr_repository.app`, `data.aws_s3_bucket.audit`). `removed { lifecycle { destroy = false } }` blocks in `main.tf` drop the old `module.ecr`/`module.audit_bucket` addresses from state without deleting anything.
- `scripts/create-env.sh <env>` is **the way to bring an environment up**. Preflight checks tools, Terraform ≥ 1.11, the account, both GitHub App secrets, and that every image tag pinned in `gitops/workloads/<env>` exists in ECR. Then it runs both bootstrap scripts, `terraform init` and `apply`, and waits until every Application is Synced/Healthy, the `public` Gateway is Programmed and the wildcard certificate is Ready. Finally it runs `mcp-server smoke` and `sre-agent whoami` as the `sre-agent` ServiceAccount (`SKIP_MODEL_CHECK=true` skips the billed Bedrock call). Extra arguments go to `terraform apply`.
- `scripts/store-github-app.sh` stores a GitHub App's credentials in Secrets Manager after checking that its permissions **exactly** match a profile. It finds the installation ID, then mints and revokes a probe token. Each app is created once in GitHub's UI; the steps are in the script header. Both apps use installation tokens that expire after an hour, so no static credential goes over the wire, and the secrets live outside Terraform and outlive environments.
  - `PROFILE=argocd`: Argo CD's **read-only** app (Contents: Read), stored in `agentic-sre/argocd/github-app`. Terraform reads it ephemerally. To rotate, re-run the script, bump `argocd_repo_secret_revision`, and apply. Repo URLs must be **HTTPS**, because GitHub App auth doesn't work over SSH.
  - `PROFILE=mcp`: the MCP server's app (Contents + Pull requests: Read and write), stored in `agentic-sre-dev/mcp-server/github-app`. External Secrets syncs it. **Only the MCP server holds a write credential; the agent holds none.** Never widen either app.
  - **No branch protection.** The repo is private on GitHub Free, so `main` isn't protected, and the PR-only path is enforced by the MCP server alone, not by GitHub. The gap is recorded in `docs/architecture.md` ("Known gap"). Don't describe the merge gate as GitHub-enforced in docs or demos.
- `scripts/bootstrap-ci.sh` creates, outside Terraform, the account's GitHub OIDC provider and three roles. CI can then authenticate while the environment is down, and the deployer role isn't created by the apply it runs. It's idempotent; run it once per account and environment, with `gh` logged in as the repo owner. **Trust is per workflow file.** It sets the repo's OIDC `sub` template to `repo` + `context` + `job_workflow_ref`, and each role's trust is a `StringLike` on one workflow on one ref:
  - `terraform-deployer-role`: `environment.yml` on `main`, AdministratorAccess.
  - `terraform-plan-role`: `ci.yml` on `refs/pull/*`, ReadOnlyAccess plus the state lock and the Argo CD app secret.
  - `image-builder-role`: `images.yml` on `main`, ECR push to the three repos.
  - **The `sub` includes IDs.** The repo was created after GitHub's July 2026 change, so `sub` uses the **immutable** format `repo:Calebache@51427452/platform-engineering@1423222068:…`. A name-only `repo:Calebache/platform-engineering:…` never matches.
  - It also allows Actions to open PRs, and sets the default token to read-only.
- `.github/workflows/` is CI/CD. Actions are pinned to commit SHAs, and tool images to digests.
  - `ci.yml` (PRs, plus dispatch) runs:
    - **Go:** gofmt, `go mod tidy -diff`, vet and `test -race` per app.
    - **Terraform:** fmt, validate and TFLint (`terraform/.tflint.hcl`).
    - **Scanners:** Trivy misconfig and Go-dependency scans (`.trivyignore.yaml`), Checkov (`.checkov.yaml`). **Every exception is scoped and has a reason.** Trivy is 0.75.0 with `--skip-check-update`, so it uses the checks built into that binary. Two failures led to this. 0.58 couldn't parse Terraform `ephemeral` blocks and **silently skipped `main.tf`** while the job passed. The newest downloaded checks bundle didn't compile on the older engine (`rego_type_error`). The one "Falling back to embedded checks" log line is expected. Trivy skips `policy/`, whose test manifests are bad on purpose.
    - **Policy:**
      - **Workloads:** `gator verify policy/gatekeeper` (the policies' tests), then `gator test` on every manifest in `gitops/workloads`, `gitops/platform` and `gitops/policies`, plus a check that `policy/examples/privileged-pod.yaml` **is rejected**.
      - **Secrets:** Conftest, for the no-Secrets-in-Git rule only.
    - **Lint:** promtool, actionlint, shellcheck.
    - **Images:** each app's image is built and given the release's Trivy scan, with `build-push.sh` and `PUSH=false` (no AWS, nothing pushed). Before this job, a broken image build context and Go stdlib CVEs only failed after merge.
    - **Plan:** an owner-only `terraform plan` when `terraform/` changed, plus Infracost if the `INFRACOST_API_KEY` secret is set. A separate `changes` job decides, so an unrelated PR shows `plan` as **skipped**, not as a green plan that never ran.
  - `images.yml` (merges to `main` that change `apps/`, or dispatch) builds, scans and pushes the changed apps with `build-push.sh`. It then opens a `ci/pin-images-<tag>` PR and dispatches `ci.yml` on it, because PRs opened with `GITHUB_TOKEN` don't trigger workflows. **Merging the pin PR is the deploy approval.**
  - **Pin PRs show two `ci` runs.** The `pull_request` one sits at "action_required", because GitHub holds workflows on PRs opened by Actions. The dispatched run on the same commit is the one that validates the PR; ignore the held one.
  - **Every workflow sets `defaults.run.shell: bash`.** GitHub's implicit shell is `bash -e` **without pipefail**. Once, `$(build-push.sh | tail -1)` hid a failed Trivy scan, and the pin PR pinned tags that were never pushed. Keep the default, and keep `build-push.sh`'s stdout for the image reference only.
  - `environment.yml` (dispatch only) runs `create-env.sh` (up) or `destroy-env.sh` (down) with `-auto-approve`, as the deployer role. Down needs the env name typed into `confirm`.
  - **Owner gate.** Every job that holds AWS credentials or writes to the repo runs `scripts/ci/require-owner.sh`: `GITHUB_ACTOR`, `GITHUB_TRIGGERING_ACTOR` and, on PRs, the PR author must be `Calebache`. It's a convention, not GitHub-enforced (see the Known gap). Read-only checks run for everyone, so the agent's PRs are still validated.
- **Policy (OPA) has one source per rule.**
  - **Workload rules** are Gatekeeper ConstraintTemplates in `gitops/policies/dev/`. **Gatekeeper enforces them at admission, and CI runs the same files with gator**, so the two layers can't disagree.
    - `K8sPlatformPodSecurity`: no privileged containers, `allowPrivilegeEscalation: false`, all capabilities dropped (only `NET_BIND_SERVICE` may be added), non-root, no host namespaces or hostPath.
    - `K8sPlatformContainerBasics`: pinned images (a tag that isn't `latest`, or a digest) and memory requests and limits.
  - **Constraints:**
    - **`deny`** in `payment-api`, `mcp-server` and `sre-agent`, on Pods and on Deployments, StatefulSets, DaemonSets, ReplicaSets, Jobs and CronJobs. A bad sync fails visibly, and agent Jobs' Pods can't get around it.
    - **`dryrun`** cluster-wide for pod security (`pod-security-audit`). Platform charts that need host access keep working, and their violations show in `status.totalViolations`.
  - **Tests:** `policy/gatekeeper/suite.yaml`, with cases in `cases/`. A new rule needs a template, a constraint and suite cases in the same change.
  - **Anything `create-env.sh` or the orchestrator launches** in those namespaces must comply, memory limits included; the check pods in `run_as_agent` do.
  - **The no-Secrets-in-Git rule** stays in Conftest (`policy/kubernetes/secrets.rego` + `secrets_test.rego`), because it's a Git rule. In the cluster, External Secrets creates Secrets legitimately.
- **Gatekeeper** (`gitops/apps/dev/gatekeeper.yaml`, chart 3.23.1, wave -1) is set up so it can't become the cluster-wide webhook outage described below.
  - **Both** validating webhooks **fail open** (`Ignore`, 3 s). That includes the namespace-label webhook, which fails closed by default, and would block Argo CD from creating namespaces while Gatekeeper is down. CI is the layer that always enforces.
  - **No mutation, no Helm hooks:** Argo CD applies the CRDs and the namespace, labelled `admission.gatekeeper.sh/ignore` and restricted.
  - **Self-rotated certificate:** Gatekeeper rotates its own webhook cert, so the Application ignores the `caBundle` and the cert Secret's data (`RespectIgnoreDifferences`).
  - **Sizing:** one replica, with PDB `minAvailable: 0` so drains and scale-down aren't blocked.
  - **Policies Application:** `gatekeeper-policies` (wave 0) syncs templates (wave 0) before constraints (wave 1, `SkipDryRunOnMissingResource`).
  - **Demo:** `kubectl apply --dry-run=server -f policy/examples/privileged-pod.yaml` is denied by `workload-pod-security`.
- `gitops/apps/<env>/` is what the root Application syncs: one Argo CD `Application` per component, plus the `platform` AppProject and the Envoy OCI repo Secret. Each Application needs `project: platform`, a sync-wave annotation and the `resources-finalizer.argocd.argoproj.io` finalizer. The AppProject's `sourceRepos` is an allow-list, so add a new chart repo in the same change as its Application.
- `gitops/platform/<env>/` holds cluster-wide config, synced by the `platform-config` Application: ClusterIssuers, the EnvoyProxy/GatewayClass, the `public` Gateway and the HTTP→HTTPS redirect.
- `apps/<name>/` holds the Go services, each its own module with a `Dockerfile` (static binary on distroless, non-root). Each `go.mod` pins `toolchain go1.26.9`, the version the Dockerfiles build with, so CI and local `go` test with the Go that ships. Bump the toolchain line and the Dockerfiles' `golang` image together. The `.dockerignore` files are allow-lists, so a new file the build needs (like the dashboard's `go:embed` templates) must be listed there too. `gitops/workloads/<env>/<name>/` holds their manifests, deployed by a wave-2 Application in `gitops/apps/<env>/`.
- `scripts/build-push.sh <app>` builds `apps/<app>` for `linux/amd64` (the nodes' architecture, even on Apple Silicon) and pushes it to ECR repo `<app>`, tagged with the 12-character commit SHA. It refuses to run if the app's directory has uncommitted changes. It builds locally, **scans with Trivy before pushing** (fixable HIGH/CRITICAL fail; an immutable tag can't be taken back), then pushes. CI's `images.yml` runs it too (`TRIVY` runs the scanner in Docker, `REQUIRE_SCAN=true`). With `PUSH=false` it only builds and scans; PR CI uses that. stdout is only the image reference. Pin a tag with `scripts/pin-image.sh <app> <tag>`, in a separate commit.
- `scripts/destroy-env.sh <env>` is **the way to tear down**. Don't run `terraform destroy` directly. Argo CD creates AWS resources Terraform doesn't track (the NLB and Route53 records), and Argo CD must process every Application's finalizer before it's uninstalled. So the script pauses the root app, deletes child Applications **wave by wave, highest first**, waiting for each. Before the first wave below the Gateway's (`GATEWAY_WAVE`, 1) it waits until no LoadBalancer Services or HTTPRoutes remain and gives external-dns two minutes. Then it deletes the root app, forgets any ECR/audit addresses still in state (`terraform state rm`, which deletes nothing in AWS), **aborts if the destroy plan touches an ECR repo or S3 bucket**, and runs `terraform destroy`. It uses a temporary kubeconfig. What survives: the state bucket, ECR images, audit records and the GitHub App secrets.

Sync waves (child Application health is enabled through a Lua check in the `argocd` module, so waves really wait). Every Application, and the root app, has a `retry` policy (10 attempts, 15 s backoff doubling to 3 m): Argo CD doesn't retry a failed automated sync for the same commit, so without it a transient failure on a fresh cluster (a CRD or webhook not ready yet) leaves bring-up stuck. A component that ships a ServiceMonitor or PrometheusRule must be in wave -1 or later.

| Wave | Contents |
|---|---|
| -3 | `platform` AppProject, Envoy OCI repo Secret |
| -2 | Envoy Gateway (installs the Gateway API CRDs), kube-prometheus-stack (installs the ServiceMonitor/PodMonitor/PrometheusRule CRDs) |
| -1 | cert-manager (needs the Gateway API CRDs for `enableGatewayAPI`), external-dns (`gateway-httproute` source), External Secrets, cluster-autoscaler (ships a ServiceMonitor), Gatekeeper |
| 0 | AWS Load Balancer Controller (its webhook cert comes from cert-manager), `gatekeeper-policies` (before the workloads they guard) |
| 1 | `platform-config` (ClusterIssuers, GatewayClass, Gateway). `destroy-env.sh` waits for the NLB and DNS records to be gone after removing this wave. |
| 2 | Workloads: payment-api, mcp-server, sre-agent. Each Application ships its own ServiceMonitor and PrometheusRule, and an HTTPRoute if it's public. |

External Secrets (wave -1) is **the only way workloads get secrets**. Terraform's `app-secret` module generates a credential as an ephemeral value and writes it to Secrets Manager under `agentic-sre-dev/...` with a write-only attribute, so it never touches Git or state. The `aws-secrets-manager` ClusterSecretStore reads it with the controller's Pod Identity, which IAM limits to the `agentic-sre-dev/` prefix. An `ExternalSecret` in the workload's namespace creates the Kubernetes Secret. The store only serves namespaces listed in its `conditions`, so add a namespace there before its first ExternalSecret. The webhook and cert-controller are off. Secrets use `recovery_window_in_days = 0`, so recreated environments can reuse the names.

Secrets Manager is the source of truth. Never edit the synced Kubernetes Secret, because External Secrets reverts it. **Terraform only seeds the first value.** Rotation is done in Secrets Manager directly, not through Terraform, and Terraform never rewrites the value, so it won't revert a rotation. Don't add a Terraform rotation knob.

Rotating the payment-api DB password changes only the stored value: Postgres applies `POSTGRES_PASSWORD` once, at first init, and both pods read env vars at start. Rotate in this order:
1. Put the new `{"username":"payments","password":"..."}` into `agentic-sre-dev/payment-api/db`, through the console or `aws secretsmanager put-secret-value`.
2. Force a sync: `kubectl -n payment-api annotate externalsecret payment-api-db force-sync=$(date +%s) --overwrite`.
3. Set the new password inside Postgres: `kubectl -n payment-api exec -it postgres-0 -- psql -U payments -d payments -c "\password payments"`.
4. Run `kubectl -n payment-api rollout restart deployment/payment-api`.

If you restart the app before step 3, every query fails authentication. Those 500s look exactly like the demo incident.

**Docs the agent reads.** `docs/architecture.md`, `docs/runbooks/<service>.md`, `apps/<service>/README.md` and `apps/payment-api/schema.sql` are served to the agent as MCP resources, read live from `main`. Keep them accurate: they're the agent's context. payment-api embeds `schema.sql` (`go:embed`), so the documented schema is the running schema.

**mcp-server** (`apps/mcp-server`, `gitops/workloads/dev/mcp-server`) is the governed gateway between the agent and the platform. It's in-cluster only: `http://mcp-server.mcp-server.svc/mcp`, Streamable HTTP.
- **One choke point.** A single receiving middleware (`middleware.go`) authenticates, authorizes, audits and counts **every** MCP method before any handler runs. Never add a path around it.
  - **Authentication:** a projected ServiceAccount token with audience `mcp-server`, verified with **TokenReview** (`auth.go`).
  - **Authorization:** the embedded OPA policy `apps/mcp-server/policy/mcp.rego`, which fails closed. Allowed callers, in-scope services, tools, prompts and the PR rules all live there. It's also served to the agent as `platform://policy/mcp`.
- **Denials** of tool calls are returned as tool results carrying the structured decision, so the agent can read `required_path`. Denials of other methods are protocol errors. `patch_deployment` exists only to be refused.
- **Tools.**
  - **Reads:** seven read-only tools for in-scope services. Metrics come from **named queries only**; the agent can't send PromQL. Logs are redacted. `get_error_patterns` groups errors by masked template and keeps host names.
  - **PR status:** `get_pull_request` reads only `agent/*` PRs. With `deployed_revision` (policy: a hex SHA only) it also returns `merge_deployed`, whether that revision includes the merge commit.
  - **Writes:** one, `create_pull_request`. Policy limits it to `gitops/workloads/<env>/<in-scope service>/`, branches named `agent/*`, no `..` in paths, and no `kind: Secret`.
- **Resources and prompts.**
  - **Resources:** a fixed catalogue (`resources.go`) mapping `platform://` URIs to repo paths.
  - **Prompts:** `investigate_incident`, `propose_remediation` and `verify_recovery` (`prompts/*.md`, embedded).
- **Its own permissions.** No AWS role. Kubernetes read access is **bound per namespace** (`rbac.yaml`), with no Secrets, plus Argo CD Applications and TokenReview. GitHub access is through its own app.
- **Adding a service** takes three changes: a RoleBinding in `rbac.yaml`, the service in `services` in `mcp.rego`, and its docs. The convention is that service X runs as Deployment X in namespace X, with Argo CD Application X.
- **Network.** A NetworkPolicy lets only the `sre-agent` namespace reach port 8080, and `monitoring` reach 9090. The VPC CNI enforces it (`enableNetworkPolicy` in the eks module). Namespaces without a NetworkPolicy remain open.
- **Output schemas are validated.** The Go SDK checks tool output against generated schemas, and a nil slice or map serializes as `null`, which fails. Every optional collection field in an output struct needs `omitempty`.
- **Tests.** `go test -race ./...` covers the policy table, pattern grouping, redaction, rollout diffs, and an end-to-end client over HTTP with authentication.
- **In-cluster check.** `mcp-server smoke` runs every check, including the denials, from a pod using the `sre-agent` ServiceAccount.

**sre-agent** (`apps/sre-agent`, `gitops/workloads/dev/sre-agent`) closes the loop: it's one image with four modes.
- **`orchestrator`** (Deployment, SA `sre-agent-orchestrator`): receives Alertmanager webhooks on `/alerts` and creates **one Job per incident** from a fixed template (`buildJob`). It de-duplicates by service and alert: no new Job while one is active, plus a 30-minute cooldown after one finishes. It has **no AWS role and no model or MCP access**; RBAC allows Jobs and reading Pods in its own namespace only. Every 30 s it reconciles finished Jobs (last 24 h) into `sre_agent_incidents{service,outcome}` and `sre_agent_incident_info{service,alert,outcome,incident_id,hypothesis}` (always 1, one series per incident), read from each pod's termination message. The hypothesis label goes into Slack, so `labelText` strips `<>&`, flattens it to one line and caps it at 200 runes. Never add free text to a label without it.
- **`investigate`** (the Job, SA `sre-agent`) runs the incident lifecycle (`worker.go`):
  - **Phases:** investigate → remediate (PR) or escalate → await merge (a deterministic poll of `get_pull_request`) → await deploy → verify → audit.
  - **Await deploy** (no model) polls `get_service_health` until the service's Argo CD Application is Synced and Healthy at a revision that **includes the merge commit** and the Deployment's rollout is complete (observed generation current, updated = ready = available = desired). "Includes" is answered by `get_pull_request {number, deployed_revision}` → `merge_deployed` (a GitHub compare, so later commits on `main` still count). It's bounded by `DEPLOY_WAIT_TIMEOUT` (15m) and recorded in `deployment_result` ("synced <sha>; rollout complete (2/2 ready)" or "deploy wait timed out after …: <last state>", which ends `not_recovered` without asking the model). Verifying on a timer alone checked the broken version, because Argo CD polls Git every ~3 minutes.
  - **Deterministic polls don't use the model's budget.** The merge and deploy polls and the recovery gate go through `pollMCP`: audited and authorised like any call, but not counted against `MAX_TOOL_CALLS`.
  - **Each phase** is a manual Claude tool loop (`loop.go`) over the MCP tools allowed in that phase. The investigate phase is never offered write tools. The phase ends when the model calls a local `submit_*` tool, so results are structured.
  - **Docs:** every phase also gets a local `read_resource {uri}` tool. It proxies to MCP `resources/read` on the same session, so the server's middleware still authenticates, authorizes and audits it. It accepts only URIs from `resources/list` (an enum in the schema, checked again locally) and counts against `MAX_TOOL_CALLS`. Reads appear in the audit record's `tool_calls`/`tools_used` as `read_resource`. The worker pre-fetches `platform://services/<svc>/runbook` into the investigate prompt.
  - **The code, not the model, decides:**
    - **The PR:** the agent trusts the recorded `create_pull_request` result over the model's report, and allows at most one PR per incident.
    - **Recovery:** it needs the model's verdict **and** `recoveryGate` (error ratio under 1%, alert not firing). There are `VERIFY_ATTEMPTS` (6) checks, `VERIFY_INTERVAL` (2m) apart, after the deploy wait; the model's `not_recovered` before the last one is recorded as "treated as not_yet" and checking continues, because `error_ratio` is a 5m rate, so it stays above the gate's 1% for a few minutes after the rollout even though the alert itself clears within about a minute. Keep attempts × interval above that lag.
    - **Escalation:** refusals, limits and missing submits escalate; nothing is retried.
  - **Model settings:** Claude Opus 4.6 on Bedrock via `bedrock-runtime` (`newBedrockModel`) with `BEDROCK_REGION` (required; never `AWS_REGION`). Adaptive thinking is set explicitly (Opus 4.6 doesn't think otherwise), effort is `high`, and the system prompt carries an explicit cache breakpoint (Bedrock has no automatic caching). History is append-only through `resp.ToParam()`. The loop never forces `tool_choice` (5.x models reject it); it nudges once instead.
  - **Recommendation:** `submit_findings` requires a `recommendation`, saved in the audit record. It says what should happen next; for an escalation, the steps a human should take, from the runbook. The dashboard shows it.
  - **Outputs:** the audit record (doc.md §16 fields) goes to `s3://agentic-sre-dev-audit-…/audit/<yyyy>/<mm>/<dd>/<incident-id>.json`, rewritten at each phase (the bucket is versioned). The outcome goes to the termination message. Outcomes are `resolved`, `no_action`, `escalated`, `pr_rejected`, `approval_timeout`, `not_recovered` and `failed`.
- **`dashboard`** (Deployment `sre-dashboard`, SA `sre-dashboard`, `dashboard.yaml`) is a read-only web page over the audit trail. It shows each incident's outcome, cause, **recommended next step**, PR, policy denials and every tool call by phase.
  - **Access:** ClusterIP only, because records hold log excerpts. Open it with `kubectl -n sre-agent port-forward svc/sre-dashboard 8088:80`.
  - **Identity:** its Pod Identity role (`sre-dashboard` in `local.workload_identities`) can only `ListBucket` with prefix `audit/` and `GetObject audit/*`, the reverse of the agent's append-only role. It has no Kubernetes token, model or MCP access. Its NetworkPolicy allows no ingress, and egress only to DNS, the Pod Identity agent and HTTPS.
  - **Data handling:** it skips `audit/whoami/` probes and records without an `execution_id`. A record unfinished 75 minutes after it started shows as **stopped**: the Job hit its one-hour deadline, or the environment went away. It re-lists every 15 s and refetches only changed ETags. Agent-written text is HTML-escaped, and the CSP allows no scripts. `/api/incidents` returns the raw records.
  - **Run locally:** `AWS_REGION=ca-central-1 AUDIT_BUCKET=agentic-sre-dev-audit-103544553431 go run ./apps/sre-agent dashboard -addr 127.0.0.1:18187`, with your SSO profile.
- **`whoami`** proves doc.md §17 from a pod running as `sre-agent`: the allowed calls succeed (STS, `PutObject audit/*`, the agent's one Bedrock model), and out-of-scope ones return `AccessDenied` (ListBucket, GetObject, PutObject outside `audit/`, ListAllMyBuckets, a different Bedrock model). `--skip-model` avoids the billed call.
- **Job safety.**
  - **Admission:** `admission-policy.yaml` (a ValidatingAdmissionPolicy on the namespace) pins every Job to the agent's ECR image, SA `sre-agent`, the `investigate` entrypoint, one container, no host namespaces, only the MCP-audience token volume, and the restricted security context.
  - **Network:** `network-policy.yaml` gives workers no ingress, and egress only to DNS, the MCP server, the Pod Identity agent (`169.254.170.23:80`) and HTTPS outside the VPC. The orchestrator admits only `monitoring`.
  - **Limits:** `backoffLimit: 0`, a 1 h deadline, and caps from `MAX_TOOL_CALLS`, `MAX_MODEL_CALLS` and `MAX_OUTPUT_TOKENS`.
- **Configuration:** the worker's settings are set on the orchestrator Deployment as `WORKER_*` env vars and passed through verbatim (the allow-list is in `orchestrator.go`; add new keys there). `WORKER_DEMO_ATTEMPT_DIRECT_FIX=true` (the dev default) tells the model to try `patch_deployment` first so the policy refusal is visible (doc.md §14); set it to `false` for normal operation.
- **Routing:** Alertmanager has two paths.
  - **To the agent:** the root route in the kube-prometheus-stack values sends only `service=~"payment-api", severity="critical", alertname!~"SreAgent.*"` to `http://sre-agent.sre-agent.svc/alerts`. The agent's own alerts are excluded by name as well as by severity, so it can't trigger itself.
  - **To humans:** the `sre-agent-humans` AlertmanagerConfig (`gitops/platform/dev/alertmanager-humans.yaml`) sends `SreAgent*` alerts to Slack. That means `SreAgentEscalation`, one alert per incident carrying the incident id, outcome, hypothesis, S3 audit key and dashboard link, sent once (`repeatInterval: 24h`, no resolved message), plus `SreAgentOrchestratorDown`. It also sends a copy of critical payment-api alerts. The operator merges it in as the first child route with `continue: true` (strategy `None`, so no namespace matcher; only the `monitoring` namespace is selected), so it never stops an alert reaching the agent.
  - **Slack webhook:** it's in Secrets Manager `agentic-sre-dev/alertmanager/slack` (`{"webhook_url": "..."}`), created by hand outside Terraform. `create-env.sh` refuses to start without it, because the ExternalSecret would stay Degraded and stall wave 1. It's deliberately **not** an `alertmanagerSpec.secrets` mount: a missing Secret volume would keep Alertmanager (wave -2) from starting before External Secrets (wave -1) exists.
  - **Checking routes:** `amtool config routes test` against the operator-generated config (`kubectl -n monitoring get secret alertmanager-kube-prometheus-stack-alertmanager-generated -o jsonpath='{.data.alertmanager\.yaml\.gz}' | base64 -d | gunzip`).
- **Tests:** `go test -race ./...` scripts whole incidents with a fake model (`anthropic.Message` built from JSON) and a fake MCP session. It covers the denial path, the single-PR limit, the gate overruling the model, escalation, refusal, timeouts, de-duplication and the Job shape.
- **Production:** the agent belongs in a separate operations cluster and account; see `docs/production-agent-topology.md`.

**payment-api** (`apps/payment-api`, `gitops/workloads/dev/payment-api`) is the demo workload:
- **API.** `POST /v1/payments` `{amount_cents, currency}` and `GET /v1/payments/{id}`. Postgres runs in the same namespace on emptyDir storage.
- **Metrics.** `http_requests_total{route,method,code}` and `payment_database_errors_total`, on port 9090. That port isn't routed publicly.
- **Logs.** JSON. A database failure logs `"database operation failed"` with the real error and a `request_id`, while the client gets a generic 500.
- **The deliberate failure** is a commit changing `DATABASE_HOST` in `deployment.yaml`. Liveness and readiness intentionally don't check the database, so the bad version rolls out and returns 500s instead of stalling unready. Malformed IDs return 404, never 5xx, so junk requests can't trip the alert.
- **Alerting.** `PaymentApiHighErrorRate` fires when 3 or more non-probe requests returned 5xx in the last minute (`increase(...[1m]) >= 3`, no `for`, its rule group evaluated every 15 s, and the agent route's `group_wait` is 10 s), so a Job starts about a minute after the first errors. It's tuned for a fast demo; production would alert on an SLO burn rate, since every firing starts a billed investigation. Keep it the only critical error alert for a service: the orchestrator de-duplicates by alert name, so a second one would start a second Job; `PaymentApiDown` fires when nothing is scraped. A `payment-loadgen` Deployment (same image, `loadgen` mode) sends about 4 req/s through `https://payments.calebache.com`, so there's always traffic to measure.
- **First 15 minutes after a fresh deploy.** The load generator starts before external-dns creates `payments.calebache.com`, so the VPC resolver caches NXDOMAIN for the zone's negative TTL (SOA minimum, 900 s). Expect `"error"` counts in its logs for up to 15 minutes; it recovers without intervention. To test before then, use `curl --resolve payments.calebache.com:443:<NLB IP>`.
- **Pod Security.** The namespace enforces the `restricted` level.
- **Testing.** Run `go test -race ./...` in `apps/payment-api`.

Observability is **kube-prometheus-stack** in the `monitoring` namespace:
- **Scraping.** Prometheus selects ServiceMonitors, PodMonitors and PrometheusRules from **every namespace** with no label requirement. To add scraping for an app, ship a ServiceMonitor/PodMonitor next to it. Platform charts have their ServiceMonitors enabled. Envoy proxies are scraped by the `envoy-proxy` PodMonitor in `gitops/platform/dev/`. That's where per-route HTTP status counters come from, including payment-api's 5xx rate.
- **What's not scraped.** EKS-managed control-plane components (controller-manager, scheduler, etcd, kube-proxy) can't be scraped. Their ServiceMonitors and default rules are disabled so they don't alert forever. Argo CD isn't scraped yet either: it's installed by Terraform before the monitoring CRDs exist.
- **Storage.** It's **ephemeral**, because the PoC has no EBS CSI driver. Retention is 2 days, and a Prometheus pod restart loses history.
- **Grafana.** ClusterIP only, with anonymous **Viewer** access. Dashboards come from Git: ConfigMaps labelled `grafana_dashboard` in any namespace. To open it: `kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80`. The admin password is in Secret `kube-prometheus-stack-grafana`. The chart randomises it on every render, so the Application ignores that field to keep the first one.
- **Operator admission webhook.** It's **disabled** (see below), so PrometheusRule syntax isn't checked at apply time. CI runs `promtool check rules` (`scripts/ci/check-prometheus-rules.sh`).

Webhooks are the main failure mode here. A broken admission webhook blocks unrelated resources cluster-wide, which is how a flaky LB controller cert once stopped cert-manager's Services from being created. So: no chart-generated self-signed webhook certs (Argo CD re-renders them on every sync; use cert-manager), and no cluster-wide mutators that aren't needed. The LB controller's Service mutator is off, and the EnvoyProxy sets `loadBalancerClass: service.k8s.aws/nlb` itself.

Ingress uses **Gateway API with Envoy Gateway**, not ingress-nginx, which was retired in March 2026 and gets no security fixes. Traffic flows: internet → NLB in the public subnets (created by the AWS Load Balancer Controller, IP targets) → Envoy pods in the private subnets → Services. TLS terminates at the Gateway with a `*.calebache.com` wildcard from Let's Encrypt (DNS-01, Route53).

To expose an app:
1. Label its namespace `gateway.calebache.com/public: "true"`. The HTTPS listener only accepts routes from labelled namespaces (deny by default).
2. Add an HTTPRoute with `parentRefs: [{name: public, namespace: envoy-gateway-system, sectionName: https}]` and a `hostnames` entry under `calebache.com`. external-dns publishes the record.

Platform gotchas:
- Charts are pinned to versions at least two weeks old. cluster-autoscaler's image is overridden to `v1.36.1` to match the cluster's minor version; bump it together with `kubernetes_version`.
- The AWS Load Balancer Controller finds the VPC by its `Name` tag (`agentic-sre-dev`), because the VPC ID changes on every recreate. Its IAM policy in `terraform/iam-policies/` is the upstream file, unmodified; replace it whole when upgrading the chart.
- Let's Encrypt prod allows **5 certificates per week** for the same set of names. When recreating the environment repeatedly, switch the Gateway's `cert-manager.io/cluster-issuer` annotation to `letsencrypt-staging`.
- external-dns runs `policy: sync` with TXT ownership (`txtOwnerId: agentic-sre-dev`, prefix `_externaldns.`), so it deletes only records it created.

Terraform **>= 1.11** is required (write-only attributes). Homebrew's `terraform` is stuck at 1.5.7; use `brew install hashicorp/tap/terraform`.

```bash
scripts/bootstrap-state-bucket.sh                                                                  # once per account
scripts/bootstrap-ci.sh                                                                            # once per account: OIDC provider + CI roles (needs gh as repo owner)
scripts/bootstrap-artifacts.sh dev                                                                 # ECR repos + audit bucket (create-env.sh runs it too)
PROFILE=argocd APP_ID=<id> PEM_FILE=<app.pem> scripts/store-github-app.sh                          # once per account, after creating the app
PROFILE=mcp    APP_ID=<id> PEM_FILE=<app.pem> scripts/store-github-app.sh                          # once per account (MCP server's PR-writing app)
terraform -chdir=terraform init -backend-config=environments/dev.s3.tfbackend -reconfigure        # select env
terraform -chdir=terraform plan  -var-file=environments/dev.tfvars
terraform -chdir=terraform apply -var-file=environments/dev.tfvars
scripts/create-env.sh dev                                                                          # bring env up and verify it end to end
scripts/destroy-env.sh dev                                                                         # tear down env (never plain terraform destroy)
terraform fmt -recursive -check terraform/
TF_DATA_DIR=/tmp/tfvalidate terraform -chdir=terraform init -backend=false && TF_DATA_DIR=/tmp/tfvalidate terraform -chdir=terraform validate   # offline validate
```

Offline validation needs its own `TF_DATA_DIR`. Once `terraform/.terraform/` has the real S3 backend configured, `init -backend=false` in place still calls STS.

Terraform gotchas in this repo:
- Each AZ gets one public and one private subnet. EKS needs control-plane subnets in **two AZs**, so both private subnets are passed to the cluster. Nodes run only in `node_availability_zones`, which dev sets to a single AZ. Keep `nat_gateway_availability_zone` in that same AZ to avoid cross-AZ data charges.
- Nothing in the private subnets has a public IP. Inbound traffic comes only through load balancers in the public subnets (tagged `kubernetes.io/role/elb`). Internal load balancers use the private subnets (`kubernetes.io/role/internal-elb`).
- The node group ignores `desired_size` because cluster-autoscaler owns it.
- **Pod density.** VPC CNI **prefix delegation** is on (vpc-cni add-on `configuration_values`), and kubelet `maxPods` is `node_max_pods` (110), set through a NodeConfig in the node launch template. The two must change together. Without prefix delegation a `t3.medium` caps at 17 pods, and the scheduler reports "Too many pods" even when CPU and memory are free.
- **Node replacement.** The node group uses `node_group_name_prefix` + `create_before_destroy`, so changes that force a new group (launch template attachment, instance type) bring the new nodes up before draining the old ones. Pods on emptyDir, like Postgres, lose their data when they move.
- **IMDSv2 with hop limit 1.** Pods can't reach instance metadata, so they can't borrow the node role or discover the region or VPC from it. Every in-cluster AWS client must get credentials from Pod Identity and have its region (and VPC, for the LB controller) configured explicitly.
- `for_each` maps use static keys (e.g. `cluster_admins`), so plans work while ARNs are still unknown. Keep that pattern.
- Resource names are `<project>-<environment>-…`, e.g. cluster `agentic-sre-dev`, role `agentic-sre-dev-sre-agent-role`.
- `terraform-deployer-role` has `AdministratorAccess` on purpose. It's contained by its trust policy (only `environment.yml` on `main`) and the owner gate. GitHub Free has no Environment reviewers for private repos. Tightening it is follow-up work.
- **No implicit cluster creator admin** (`bootstrap_cluster_creator_admin_permissions = false`). Every admin comes from `cluster_admins`: the CI deployer role, plus `cluster_admin_principal_arns` in tfvars (your SSO role, ARN with its path). So access is the same whether you or CI created the cluster, and the creator's entry can't collide with the deployer's. If your SSO permission set is re-provisioned, update the ARN.
- **Single-apply bootstrap.** The `kubernetes` and `helm` providers are configured from `module.eks` outputs and authenticate with `aws eks get-token`, so wherever Terraform runs needs the AWS CLI. This works for create and destroy. If a change would *replace* the EKS cluster, apply `-target=module.eks` first, then a full apply.
- The `argocd` module waits on all of `module.eks` (nodes, CoreDNS, access entries). The root Application is installed through the `argocd-apps` Helm chart, not `kubernetes_manifest`, because the Application CRD doesn't exist at plan time.
- The GitHub App credentials are read with an **ephemeral** `aws_secretsmanager_secret_version` and written with `data_wo`, so they never appear in state or plan files. Don't switch it to a data source or a plain `data` attribute.
- **Destroy relies on finalizers and ordering.** Deleting an Application cascades only if it has `resources-finalizer.argocd.argoproj.io`. `scripts/destroy-env.sh` removes the Gateway and workloads before the controllers go. Without that ordering, Argo-created LoadBalancers and Route53 records are orphaned, and the VPC fails to delete.
- The root app syncs from GitHub `main`. Until this repo is pushed, it shows a sync error; that doesn't fail the apply.
- **PR plans use `-refresh=false`.** The plan role has no EKS access entry, so it plans config against recorded state and doesn't detect drift. It has `secretsmanager:GetSecretValue` on the Argo CD app secret only, because the ephemeral read happens at plan time.
- The sre-agent role allows `bedrock:InvokeModel` on one inference profile and its foundation model only (see "Bedrock client and IAM" below).

The PoC's purpose is to show that an AI SRE agent can investigate incidents and propose fixes while staying inside deterministic platform controls. The agent is a workload, not a trusted administrator. Target lifecycle: Provision → Deploy → Observe → Detect → Investigate → Propose Fix → Validate → Approve → Deploy → Verify → Audit.

## Decisions made

- **AWS:** account `103544553431`, region `ca-central-1` (Canada Central). The Terraform state S3 bucket doesn't exist yet and is created by `scripts/bootstrap-state-bucket.sh`.
- **GitHub:** `Calebache/platform-engineering` (https://github.com/Calebache/platform-engineering). Scope OIDC trust policies to this repo.
- **DNS:** `calebache.com`, hosted in Route53. external-dns and cert-manager use it.
- **Language:** Go for the MCP server, the SRE agent and `payment-api`.
- **OPA** runs at three levels:
  - **Runtime authorization:** the MCP server's embedded policy decides every agent request.
  - **Admission:** Gatekeeper enforces the workload rules (added 2026-10-19; this replaces the earlier "CI only, no Gatekeeper" decision).
  - **CI:** gator runs the same Gatekeeper policies, and Conftest the no-Secrets-in-Git rule.
- **Audit trail:** stored in S3.
- **Agent LLM:** **Claude Opus 4.6** on Amazon Bedrock, through `bedrock-runtime` (InvokeModel) and the global inference profile `global.anthropic.claude-opus-4-6-v1`. The plan was Opus 5.5 through Bedrock's Mantle Messages endpoint. As of 2026-10-17, though, this account gets "not available for this account" for **every** model on Mantle, and for Opus 4.7, 4.8, 5 and 5.5 and Sonnet 5 on `bedrock-runtime` too. Opus 4.6, Sonnet 4.6, Sonnet 4.5 and Haiku 4.5 work. Unlocking the newer models is an AWS account matter. To move up later, change `bedrock_inference_profile`, `bedrock_foundation_model` and `WORKER_MODEL_ID` together; for Mantle-only models, also switch the client back to `bedrock.NewMantleClient` and the IAM action to `bedrock-mantle:CreateInference`.
- **Bedrock client and IAM.** The Go client is `anthropic.NewClient(bedrock.WithConfig(cfg))` from `github.com/anthropics/anthropic-sdk-go`, with the region from `BEDROCK_REGION` (never `AWS_REGION`); it speaks the Messages API, so the tool loop is the same. Opus 4.6 needs adaptive thinking set **explicitly** (`OfAdaptive`), unlike 5.x. The agent role allows `bedrock:InvokeModel` on **only** that inference profile, plus the foundation model in any region **only through that profile** (`bedrock:InferenceProfileArn` condition). Calling any other model, even one the account can use, is `AccessDenied`, and `whoami` checks this.
- **Bedrock region is `ca-central-1`**, the cluster's own (`bedrock_region`). `bedrock-runtime` exists there, and the **global** inference profile routes inference to wherever capacity is, so inference may run outside Canada. (The earlier us-east-1 setting was only because `bedrock-mantle.ca-central-1.api.aws` doesn't exist: NXDOMAIN, checked 2026-10-17.)
- **Nodes:** EKS managed node group, desired 2, max 3, with autoscaling.
- **Scope:** all 20 success criteria in doc.md §20, built incrementally.
- **Local Dell server:** out of scope; ignore it.

## Planned architecture (ownership boundaries matter)

- **Terraform owns the AWS substrate only.** That covers the VPC (single region; nodes in **private subnets** with one NAT gateway in the node AZ; public subnets only for load balancers and the NAT; free S3 gateway endpoint), EKS with two small EC2 nodes, IAM, EKS Pod Identity, Route53 permissions, and an S3 remote state backend with S3 locking. ECR repos and the audit bucket are created by `scripts/bootstrap-artifacts.sh`, and the GitHub OIDC provider and CI roles by `scripts/bootstrap-ci.sh`. Terraform only reads them, so they outlive environments. Terraform's Kubernetes role **ends at installing Argo CD and its root application**. Don't add Kubernetes platform apps to Terraform.
- **Argo CD owns all steady-state Kubernetes resources**, through App-of-Apps: Envoy Gateway (Gateway API), the AWS Load Balancer Controller, cluster-autoscaler, cert-manager, external-dns, observability (Prometheus, Grafana, optional Loki), the MCP server, the agent runtime and the demo app. The sync-wave table above is the source of truth for ordering.
- **GitHub Actions** handles CI/CD and authenticates to AWS **only through OIDC**, never with static keys. Trust policies are scoped to one workflow file on one ref (see `bootstrap-ci.sh`). Each duty gets its own role (`terraform-deployer-role`, `image-builder-role`, `agent-runtime-role`, `external-dns-role`). The Terraform deployer identity must never be reused by workloads.
- **Images** go in ECR repos `mcp-server`, `sre-agent` and `payment-api`, one repo per app, named after its `apps/` directory. Tags are the Git commit SHA, ideally immutable, and never `latest`. Images are scanned in CI and deployed via Argo CD.
- **PR validation** covers formatting, linting, unit tests, `terraform validate`/`plan`, TFLint, Trivy, Checkov, OPA (gator and Conftest) and Infracost. Privileged changes need a human approval gate.
- **MCP server** is the agent's governed tool gateway. Example tools: `get_service_health`, `get_recent_logs`, `get_error_patterns`, `get_recent_deployments`, `get_git_changes`, `get_metrics`, `get_incident_context`, `create_pull_request`. It enforces authorization itself, never through the LLM. It also does context engineering: it returns grouped, deduplicated summaries first, with progressively deeper evidence on request. Raw data must stay reachable.
- **Agent runtime** runs on Kubernetes: an orchestrator service, Jobs, CronJobs and ephemeral Jobs. Permissions are mostly read-only, plus creating PRs. The agent must never modify IAM, delete infrastructure, patch production directly, push to main or bypass GitOps. A denied direct mutation should return `{"allowed": false, "reason": ..., "required_path": "create_pull_request"}`.
- **Demo app** is `payment-api`. Its deliberate failure is a wrong `DATABASE_HOST` that causes HTTP 500s. Prometheus detects the failure and starts the incident flow. After Argo CD deploys the fix, the agent must check that the service actually recovered (5xx rate, readiness, error logs) before the incident counts as resolved.
- **Audit trail**: every agent execution writes a JSON record. It holds the tools used, hypothesis, evidence, proposed action, policy and security results, human approval, deployment result and final service health. See doc.md §16 for the schema.

## Intentional failure demos

These must stay demonstrable, so don't "fix" them away:
- The agent's direct production patch is denied.
- An AWS call outside the agent's IAM scope returns `AccessDenied` (prove the identity with `aws sts get-caller-identity`).
- A manifest with `securityContext.privileged: true` fails the OPA check, in CI (gator) and at admission (Gatekeeper).

## Environment lifecycle

The environment is disposable and meant to be created for a demo and torn down afterward. `scripts/create-env.sh <env>` builds everything (one `terraform apply` that bootstraps Argo CD) and verifies it. `scripts/destroy-env.sh <env>` removes it. Images and audit records survive the cycle, so a recreated environment runs the same pinned images. Production hardening (multi-AZ nodes, one NAT gateway per AZ, interface VPC endpoints, HA) is documented as future work and is not a PoC requirement. The local Dell server is for development only and must not be exposed publicly.
