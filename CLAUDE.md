# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Current state

[doc.md](doc.md) is the implementation summary for an **Agentic SRE Platform PoC** on AWS; read it before building anything. So far there's the Terraform foundation, the Argo CD bootstrap (one `terraform apply` does both), the platform Applications (Envoy Gateway, AWS Load Balancer Controller, cluster-autoscaler, cert-manager, external-dns, plus the public Gateway), observability (kube-prometheus-stack), External Secrets, and the `payment-api` demo workload. CI, the MCP server and the SRE agent come next. Update this file as they land.

## Layout and commands

Terraform follows a **root module + one module per resource type + per-environment tfvars** layout:

- `terraform/` (root): `main.tf` only wires modules together. `policies.tf` holds the IAM permission policies passed into modules. `variables.tf` leaves environment-specific values without defaults, so they must come from tfvars. The backend is a partial `backend "s3" {}`.
- `terraform/modules/<type>/`: `network`, `eks`, `ecr`, `s3-bucket`, `github-oidc`, `pod-identity-role`, `argocd` and `app-secret`. Each has `main.tf`, `variables.tf`, `outputs.tf` and `versions.tf`. Put new resource types in a new module, not in the root. `pod-identity-role` binds one IAM role to one namespace/ServiceAccount; add a new workload identity as an entry in `local.workload_identities` in `main.tf`.
- `terraform/environments/<env>.tfvars` sets an environment's values. `<env>.s3.tfbackend` sets its state key. To add an environment, copy both and change the values. Only the first environment in an account sets `create_github_oidc_provider = true`, and ECR repo names must not overlap within an account.
- `scripts/bootstrap-state-bucket.sh` creates the S3 state bucket. Terraform deliberately **doesn't manage its own state bucket**, which avoids local state and the chicken-and-egg migration. The script is idempotent: re-running it re-applies versioning, encryption, public-access block, lifecycle and the TLS-only policy. Override with the `ACCOUNT_ID`, `REGION` and `BUCKET` env vars. Run it once per account before the first `terraform init`. The bucket outlives every environment.
- `scripts/store-argocd-github-app.sh`: Argo CD reads the private repo through a **read-only GitHub App** (Contents: Read-only, installed on this repo only), not a deploy key. Argo CD mints installation tokens that expire after an hour, so no static credential goes over the wire. The app is created once in GitHub's UI; the steps are in the script header. Run the script with `APP_ID` and `PEM_FILE`. It finds the installation ID, mints and revokes a probe token to check that permissions are exactly read-only, and stores `{appId, installationId, privateKey}` in Secrets Manager (`agentic-sre/argocd/github-app`). The secret lives outside Terraform and outlives environments. To rotate, re-run it with a new `.pem`, bump `argocd_repo_secret_revision`, and apply. Repo URLs must be **HTTPS** (`https://github.com/Calebache/platform-engineering.git`), because GitHub App auth doesn't work over SSH. The SRE agent's PR-writing identity will be a **separate** app with write permissions; never widen this one.
- `gitops/apps/<env>/` is what the root Application syncs: one Argo CD `Application` per component, plus the `platform` AppProject and the Envoy OCI repo Secret. Each Application needs `project: platform`, a sync-wave annotation and the `resources-finalizer.argocd.argoproj.io` finalizer. The AppProject's `sourceRepos` is an allow-list, so add a new chart repo in the same change as its Application.
- `gitops/platform/<env>/` holds cluster-wide config, synced by the `platform-config` Application: ClusterIssuers, the EnvoyProxy/GatewayClass, the `public` Gateway and the HTTP→HTTPS redirect.
- `apps/<name>/` holds the Go services, each its own module with a `Dockerfile` (static binary on distroless, non-root). `gitops/workloads/<env>/<name>/` holds their manifests, deployed by a wave-2 Application in `gitops/apps/<env>/`.
- `scripts/build-push.sh <app>` builds `apps/<app>` for `linux/amd64` (the nodes' architecture, even on Apple Silicon) and pushes it to ECR repo `<app>`, tagged with the 12-character commit SHA. It refuses to run if the app's directory has uncommitted changes. It's a stopgap until CI builds images. After pushing, put the tag into the workload's manifests in a separate commit.
- `scripts/destroy-env.sh <env>` is **the way to tear down**. Don't run `terraform destroy` directly. Argo CD creates AWS resources Terraform doesn't track (the NLB and Route53 records), so the script first pauses the root app, deletes every wave ≥ 1 Application (`TEARDOWN_FROM_WAVE`), and waits until LoadBalancer Services and HTTPRoutes are gone while the controllers are still running. Then it runs `terraform destroy`. It uses a temporary kubeconfig.

Sync waves (child Application health is enabled through a Lua check in the `argocd` module, so waves really wait):

| Wave | Contents |
|---|---|
| -3 | `platform` AppProject, Envoy OCI repo Secret |
| -2 | Envoy Gateway (installs the Gateway API CRDs), kube-prometheus-stack (installs the ServiceMonitor/PodMonitor/PrometheusRule CRDs), cluster-autoscaler |
| -1 | cert-manager (needs the Gateway API CRDs for `enableGatewayAPI`), external-dns (`gateway-httproute` source) |
| 0 | AWS Load Balancer Controller (its webhook cert comes from cert-manager) |
| 1 | `platform-config` (ClusterIssuers, GatewayClass, Gateway). This is the first wave `destroy-env.sh` removes. |
| 2 | Workloads: payment-api (MCP server and agent runtime planned). Each Application ships its own HTTPRoute, ServiceMonitor and PrometheusRule. |

External Secrets (wave -1) is **the only way workloads get secrets**. Terraform's `app-secret` module generates a credential as an ephemeral value and writes it to Secrets Manager under `agentic-sre-dev/...` with a write-only attribute, so it never touches Git or state. The `aws-secrets-manager` ClusterSecretStore reads it with the controller's Pod Identity, which IAM limits to the `agentic-sre-dev/` prefix. An `ExternalSecret` in the workload's namespace creates the Kubernetes Secret. The store only serves namespaces listed in its `conditions`, so add a namespace there before its first ExternalSecret. The webhook and cert-controller are off. Secrets use `recovery_window_in_days = 0`, so recreated environments can reuse the names.

Secrets Manager is the source of truth. Never edit the synced Kubernetes Secret, because External Secrets reverts it. **Terraform only seeds the first value.** Rotation is done in Secrets Manager directly, not through Terraform, and Terraform never rewrites the value, so it won't revert a rotation. Don't add a Terraform rotation knob.

Rotating the payment-api DB password changes only the stored value: Postgres applies `POSTGRES_PASSWORD` once, at first init, and both pods read env vars at start. Rotate in this order:
1. Put the new `{"username":"payments","password":"..."}` into `agentic-sre-dev/payment-api/db`, through the console or `aws secretsmanager put-secret-value`.
2. Force a sync: `kubectl -n payment-api annotate externalsecret payment-api-db force-sync=$(date +%s) --overwrite`.
3. Set the new password inside Postgres: `kubectl -n payment-api exec -it postgres-0 -- psql -U payments -d payments -c "\password payments"`.
4. Run `kubectl -n payment-api rollout restart deployment/payment-api`.

If you restart the app before step 3, every query fails authentication. Those 500s look exactly like the demo incident.

**payment-api** (`apps/payment-api`, `gitops/workloads/dev/payment-api`) is the demo workload:
- **API.** `POST /v1/payments` `{amount_cents, currency}` and `GET /v1/payments/{id}`. Postgres runs in the same namespace on emptyDir storage.
- **Metrics.** `http_requests_total{route,method,code}` and `payment_database_errors_total`, on port 9090. That port isn't routed publicly.
- **Logs.** JSON. A database failure logs `"database operation failed"` with the real error and a `request_id`, while the client gets a generic 500.
- **The deliberate failure** is a commit changing `DATABASE_HOST` in `deployment.yaml`. Liveness and readiness intentionally don't check the database, so the bad version rolls out and returns 500s instead of stalling unready. Malformed IDs return 404, never 5xx, so junk requests can't trip the alert.
- **Alerting.** `PaymentApiHighErrorRate` fires when more than 5% of non-probe requests are 5xx for 2 minutes; `PaymentApiDown` fires when nothing is scraped. A `payment-loadgen` Deployment (same image, `loadgen` mode) sends about 4 req/s through `https://payments.calebache.com`, so there's always traffic to measure.
- **First 15 minutes after a fresh deploy.** The load generator starts before external-dns creates `payments.calebache.com`, so the VPC resolver caches NXDOMAIN for the zone's negative TTL (SOA minimum, 900 s). Expect `"error"` counts in its logs for up to 15 minutes; it recovers without intervention. To test before then, use `curl --resolve payments.calebache.com:443:<NLB IP>`.
- **Pod Security.** The namespace enforces the `restricted` level.
- **Testing.** Run `go test -race ./...` in `apps/payment-api`.

Observability is **kube-prometheus-stack** in the `monitoring` namespace:
- **Scraping.** Prometheus selects ServiceMonitors, PodMonitors and PrometheusRules from **every namespace** with no label requirement. To add scraping for an app, ship a ServiceMonitor/PodMonitor next to it. Platform charts have their ServiceMonitors enabled. Envoy proxies are scraped by the `envoy-proxy` PodMonitor in `gitops/platform/dev/`. That's where per-route HTTP status counters come from, including payment-api's 5xx rate.
- **What's not scraped.** EKS-managed control-plane components (controller-manager, scheduler, etcd, kube-proxy) can't be scraped. Their ServiceMonitors and default rules are disabled so they don't alert forever. Argo CD isn't scraped yet either: it's installed by Terraform before the monitoring CRDs exist.
- **Storage.** It's **ephemeral**, because the PoC has no EBS CSI driver. Retention is 2 days, and a Prometheus pod restart loses history.
- **Grafana.** ClusterIP only, with anonymous **Viewer** access. Dashboards come from Git: ConfigMaps labelled `grafana_dashboard` in any namespace. To open it: `kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80`. The admin password is in Secret `kube-prometheus-stack-grafana`. The chart randomises it on every render, so the Application ignores that field to keep the first one.
- **Operator admission webhook.** It's **disabled** (see below), so PrometheusRule syntax isn't checked at apply time. CI must run `promtool check rules`.

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
APP_ID=<id> PEM_FILE=<app.pem> scripts/store-argocd-github-app.sh                                  # once per account, after creating the app
terraform -chdir=terraform init -backend-config=environments/dev.s3.tfbackend -reconfigure        # select env
terraform -chdir=terraform plan  -var-file=environments/dev.tfvars
terraform -chdir=terraform apply -var-file=environments/dev.tfvars
scripts/destroy-env.sh dev                                                                         # tear down env (never plain terraform destroy)
terraform fmt -recursive -check terraform/
TF_DATA_DIR=/tmp/tfvalidate terraform -chdir=terraform init -backend=false && TF_DATA_DIR=/tmp/tfvalidate terraform -chdir=terraform validate   # offline validate
```

Offline validation needs its own `TF_DATA_DIR`. Once `terraform/.terraform/` has the real S3 backend configured, `init -backend=false` in place still calls STS.

Terraform gotchas in this repo:
- Each AZ gets one public and one private subnet. EKS needs control-plane subnets in **two AZs**, so both private subnets are passed to the cluster. Nodes run only in `node_availability_zones`, which dev sets to a single AZ. Keep `nat_gateway_availability_zone` in that same AZ to avoid cross-AZ data charges.
- Nothing in the private subnets has a public IP. Inbound traffic comes only through load balancers in the public subnets (tagged `kubernetes.io/role/elb`). Internal load balancers use the private subnets (`kubernetes.io/role/internal-elb`).
- The node group ignores `desired_size` because cluster-autoscaler owns it.
- `for_each` maps use static keys (e.g. `cluster_admins`, and the `role|policy` keys in `github-oidc`), so plans work while ARNs and policy JSON are still unknown. Keep that pattern.
- Resource names are `<project>-<environment>-…`, e.g. cluster `agentic-sre-dev`, role `agentic-sre-dev-sre-agent-role`.
- `terraform-deployer-role` has `AdministratorAccess` on purpose. It's contained by its trust policy, which allows only the `dev` GitHub Environment, and that Environment holds the approval gate. Tightening it is follow-up work.
- **Single-apply bootstrap.** The `kubernetes` and `helm` providers are configured from `module.eks` outputs and authenticate with `aws eks get-token`, so wherever Terraform runs needs the AWS CLI. This works for create and destroy. If a change would *replace* the EKS cluster, apply `-target=module.eks` first, then a full apply.
- The `argocd` module waits on all of `module.eks` (nodes, CoreDNS, access entries). The root Application is installed through the `argocd-apps` Helm chart, not `kubernetes_manifest`, because the Application CRD doesn't exist at plan time.
- The GitHub App credentials are read with an **ephemeral** `aws_secretsmanager_secret_version` and written with `data_wo`, so they never appear in state or plan files. Don't switch it to a data source or a plain `data` attribute.
- **Destroy relies on finalizers and ordering.** Deleting an Application cascades only if it has `resources-finalizer.argocd.argoproj.io`. `scripts/destroy-env.sh` removes the Gateway and workloads before the controllers go. Without that ordering, Argo-created LoadBalancers and Route53 records are orphaned, and the VPC fails to delete.
- The root app syncs from GitHub `main`. Until this repo is pushed, it shows a sync error; that doesn't fail the apply.
- **CI follow-up.** The plan role will need `secretsmanager:GetSecretValue` on the GitHub App secret (ephemeral resources open during plan) and an EKS access entry before CI can plan the Argo CD module.
- The sre-agent role has `bedrock-mantle:CreateInference` on `*` (limited to `bedrock_region`) for now. Scope it to the model ARN once the resource ARN format is confirmed.

The PoC's purpose is to show that an AI SRE agent can investigate incidents and propose fixes while staying inside deterministic platform controls. The agent is a workload, not a trusted administrator. Target lifecycle: Provision → Deploy → Observe → Detect → Investigate → Propose Fix → Validate → Approve → Deploy → Verify → Audit.

## Decisions made

- **AWS:** account `103544553431`, region `ca-central-1` (Canada Central). The Terraform state S3 bucket doesn't exist yet and is created by `scripts/bootstrap-state-bucket.sh`.
- **GitHub:** `Calebache/platform-engineering` (https://github.com/Calebache/platform-engineering). Scope OIDC trust policies to this repo.
- **DNS:** `calebache.com`, hosted in Route53. external-dns and cert-manager use it.
- **Language:** Go for the MCP server, the SRE agent and `payment-api`.
- **OPA:** enforced in CI only, via Conftest. No in-cluster Gatekeeper.
- **Audit trail:** stored in S3.
- **Agent LLM:** Claude Opus 5.5 on Amazon Bedrock (model ID `anthropic.claude-opus-5-5`). The agent authenticates through EKS Pod Identity, with no API keys. Its IAM role allows only invoking that model or inference profile. The Go client is the Bedrock Mantle client from `github.com/anthropics/anthropic-sdk-go` (`bedrock.NewMantleClient`). The IAM action is `bedrock-mantle:CreateInference`. Opus 5.5 access on Bedrock may need enabling in the console.
- **Bedrock region is `us-east-1`, not the cluster region.** The cluster stays in `ca-central-1`, but `bedrock-mantle.ca-central-1.api.aws` doesn't exist (NXDOMAIN, checked 2026-10-17), even though Anthropic's docs list the region. So the agent's client must set `AWSRegion: "us-east-1"` explicitly, from the `bedrock_region` Terraform variable / output. It must never fall back to `AWS_REGION`, which in the pod points at the cluster region. Inference therefore runs in the US. The agent role's IAM policy only allows `CreateInference` when `aws:RequestedRegion` equals `bedrock_region`.
- **Nodes:** EKS managed node group, desired 2, max 3, with autoscaling.
- **Scope:** all 20 success criteria in doc.md §20, built incrementally.
- **Local Dell server:** out of scope; ignore it.

## Planned architecture (ownership boundaries matter)

- **Terraform owns the AWS substrate only.** That covers the VPC (single region; nodes in **private subnets** with one NAT gateway in the node AZ; public subnets only for load balancers and the NAT; free S3 gateway endpoint), EKS with two small EC2 nodes, ECR, IAM, GitHub OIDC, EKS Pod Identity, Route53 permissions, and an S3 remote state backend with S3 locking. Terraform's Kubernetes role **ends at installing Argo CD and its root application**. Don't add Kubernetes platform apps to Terraform.
- **Argo CD owns all steady-state Kubernetes resources**, through App-of-Apps: Envoy Gateway (Gateway API), the AWS Load Balancer Controller, cluster-autoscaler, cert-manager, external-dns, observability (Prometheus, Grafana, optional Loki), the MCP server, the agent runtime and the demo app. The sync-wave table above is the source of truth for ordering.
- **GitHub Actions** handles CI/CD and authenticates to AWS **only through OIDC**, never with static keys. Trust policies are scoped by org, repo, branch and environment. Each duty gets its own role (`terraform-deployer-role`, `image-builder-role`, `agent-runtime-role`, `external-dns-role`). The Terraform deployer identity must never be reused by workloads.
- **Images** go in ECR repos `mcp-server`, `sre-agent` and `payment-api`, one repo per app, named after its `apps/` directory. Tags are the Git commit SHA, ideally immutable, and never `latest`. Images are scanned in CI and deployed via Argo CD.
- **PR validation** covers formatting, linting, unit tests, `terraform validate`/`plan`, TFLint, Trivy, Checkov, OPA/Conftest and Infracost. Privileged changes need a human approval gate.
- **MCP server** is the agent's governed tool gateway. Example tools: `get_service_health`, `get_recent_logs`, `get_error_patterns`, `get_recent_deployments`, `get_git_changes`, `get_metrics`, `get_incident_context`, `create_pull_request`. It enforces authorization itself, never through the LLM. It also does context engineering: it returns grouped, deduplicated summaries first, with progressively deeper evidence on request. Raw data must stay reachable.
- **Agent runtime** runs on Kubernetes: an orchestrator service, Jobs, CronJobs and ephemeral Jobs. Permissions are mostly read-only, plus creating PRs. The agent must never modify IAM, delete infrastructure, patch production directly, push to main or bypass GitOps. A denied direct mutation should return `{"allowed": false, "reason": ..., "required_path": "create_pull_request"}`.
- **Demo app** is `payment-api`. Its deliberate failure is a wrong `DATABASE_HOST` that causes HTTP 500s. Prometheus detects the failure and starts the incident flow. After Argo CD deploys the fix, the agent must check that the service actually recovered (5xx rate, readiness, error logs) before the incident counts as resolved.
- **Audit trail**: every agent execution writes a JSON record. It holds the tools used, hypothesis, evidence, proposed action, policy and security results, human approval, deployment result and final service health. See doc.md §16 for the schema.

## Intentional failure demos

These must stay demonstrable, so don't "fix" them away:
- The agent's direct production patch is denied.
- An AWS call outside the agent's IAM scope returns `AccessDenied` (prove the identity with `aws sts get-caller-identity`).
- A manifest with `securityContext.privileged: true` fails the OPA check.

## Environment lifecycle

The environment is disposable and meant to be created for a demo and torn down afterward. `terraform apply` builds everything and bootstraps Argo CD. `scripts/destroy-env.sh <env>` removes it. Production hardening (multi-AZ nodes, one NAT gateway per AZ, interface VPC endpoints, HA) is documented as future work and is not a PoC requirement. The local Dell server is for development only and must not be exposed publicly.
