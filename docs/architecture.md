# Agentic SRE Platform — Architecture

This is the as-built architecture of the PoC described in [doc.md](../doc.md).
It is also served to the SRE agent as the MCP resource
`platform://docs/architecture`, so keep it accurate when the platform changes.

## Principle

The agent is a workload, not an administrator. It reasons; the platform
decides. Every consequential action passes through deterministic controls the
agent cannot change: IAM, Kubernetes RBAC, the MCP server's OPA policy, CI
checks, and human approval of pull requests.

Lifecycle: **Provision → Deploy → Observe → Detect → Investigate → Propose
Fix → Validate → Approve → Deploy → Verify → Audit.**

## Environment

| | |
|---|---|
| AWS account / region | `103544553431` / `ca-central-1` |
| Cluster | EKS `agentic-sre-dev`, managed node group, 2–3 × `t3.medium` |
| Git repository | `Calebache/platform-engineering` (private) |
| Public domain | `*.calebache.com` (Route53) |
| Agent model | Claude Opus 4.6 on Amazon Bedrock (`bedrock-runtime`, global inference profile, called in `ca-central-1`) |

The environment is disposable: created and verified with
`scripts/create-env.sh dev` (one `terraform apply`), removed with
`scripts/destroy-env.sh dev`. Images and audit records survive the cycle.

## Ownership boundaries

| Layer | Owner | Contents |
|---|---|---|
| Cloud substrate | Terraform (`terraform/`) | VPC, EKS, IAM, Pod Identity, Secrets Manager entries |
| GitOps bootstrap | Terraform `argocd` module | Argo CD and one root Application, nothing more |
| Everything in the cluster | Argo CD (`gitops/`) | Platform controllers, config, workloads |
| Out-of-band, outlives environments | Scripts (`scripts/`) | Terraform state bucket, ECR repositories, audit bucket, GitHub OIDC provider and CI roles, GitHub App credentials |

Nothing in the cluster is changed by hand or by Terraform after bootstrap; a
change is a commit.

## Network

```
Internet ──> NLB (public subnets) ──> Envoy (private subnets) ──> Services
Private subnets ──> NAT gateway (one, node AZ) ──> Internet
Private subnets ──> S3 gateway endpoint (no NAT charge)
```

- VPC `10.0.0.0/16`; a public and a private `/20` in each of two AZs. Nodes
  run in one AZ (`ca-central-1a`).
- Nodes have no public IPs. Inbound traffic arrives only through load
  balancers in the public subnets.
- Ingress is the Gateway API (Envoy Gateway); one `public` Gateway terminates
  TLS with a Let's Encrypt `*.calebache.com` wildcard. Namespaces must opt in
  (label `gateway.calebache.com/public=true`) before routes can attach.
- VPC CNI prefix delegation gives 110 pods per node. Network policy
  enforcement is enabled; namespaces that hold sensitive endpoints (the MCP
  server) deny ingress by default.

## Identity

| Who | How it authenticates | What it may do |
|---|---|---|
| Humans | AWS SSO → EKS access entry | Cluster admin (dev) |
| GitHub Actions | OIDC → STS; each role trusts one workflow file on one ref | `terraform-deployer` (`environment.yml`, owner-only), `terraform-plan` (`ci.yml` on PRs), `image-builder` (`images.yml` on main) |
| Workloads | EKS Pod Identity, one role per ServiceAccount | Only their own AWS API calls |
| Audit dashboard | EKS Pod Identity (`sre-dashboard`) | List and read `audit/*`; nothing else |
| Argo CD → GitHub | Read-only GitHub App, hourly tokens | Read this repo |
| MCP server → GitHub | Separate GitHub App | Read repo, open PRs from `agent/*` branches |
| SRE agent → MCP server | ServiceAccount token, verified with TokenReview | Whatever the OPA policy allows |

Nodes require IMDSv2 with a hop limit of 1, so pods cannot borrow the node
role. No static AWS keys exist anywhere.

## Secrets

Secrets Manager is the source of truth. Terraform seeds generated values
write-only (never in state); External Secrets syncs them into Kubernetes
Secrets with a Pod Identity role limited to `agentic-sre-dev/*`. Rotation is
done in Secrets Manager. Nothing secret is stored in Git.

## Platform components (Argo CD sync waves)

| Wave | Component | Role |
|---|---|---|
| -3 | `platform` AppProject | Allow-list of source repositories |
| -2 | Envoy Gateway, kube-prometheus-stack | Gateway API, monitoring CRDs |
| -1 | cert-manager, external-dns, External Secrets, cluster-autoscaler | TLS, DNS records, secrets, node scaling |
| 0 | AWS Load Balancer Controller | NLB for the Gateway |
| 1 | platform-config | ClusterIssuers, GatewayClass, Gateway, ClusterSecretStore |
| 2 | Workloads | payment-api, mcp-server, sre-agent |

## Observability

Prometheus scrapes every ServiceMonitor/PodMonitor in the cluster; Envoy
proxies and each workload expose request counters by status code.
Alertmanager receives alerts such as `PaymentApiHighErrorRate`; it will route
them to the SRE agent. Grafana is internal only.

## The agent loop

```
Alertmanager ──webhook──> orchestrator ──creates──> incident Job (one per incident)
                         (no AWS role,               │
                          no model access)           ├──MCP──> MCP server ──> Kubernetes (read)
                                                     │                    ├──> Prometheus (named queries)
                                                     │                    ├──> Argo CD (read)
                                                     │                    └──> GitHub (read, PRs)
                                                     ├──> Bedrock (Claude Opus 4.6, global profile)
                                                     └──> S3 audit record
```

1. A critical alert for an in-scope service reaches the **orchestrator**,
   which de-duplicates it and creates one **Job** for the incident.
2. The Job's worker investigates only through MCP tools, starting with the
   `investigate_incident` prompt and `get_incident_context`, and reads the
   service runbook resource.
3. It submits a hypothesis with evidence and the runbook's path: fix through
   Git, escalate to a human, or no action.
4. To fix, it proposes a pull request. Direct mutation is refused by policy
   (`patch_deployment` → `required_path: create_pull_request`); in demo mode
   the agent is told to try it first, so the refusal is visible.
5. A human reviews and merges the PR; the worker polls for the merge.
6. Argo CD deploys the fix; the worker verifies recovery. Both the model's
   verdict and a deterministic check (error ratio under 1%, alert not
   firing) must agree before the incident is resolved.
7. The worker writes the audit record to S3 at every phase, and reports its
   outcome in the Job's termination message. Anything other than
   "resolved" or "no action" raises `SreAgentEscalation`.

### Incident Jobs

- **Identity.** Workers run as `sre-agent`: one Bedrock model (Opus 4.6,
  through its inference profile) and `PutObject` on `audit/*` only, no Kubernetes RBAC, no GitHub credential.
  The only token mounted is a short-lived one for the MCP server's
  audience. The orchestrator runs as `sre-agent-orchestrator`, which can
  create Jobs and read their Pods in its own namespace and nothing else.
- **Shape is pinned.** A `ValidatingAdmissionPolicy` allows Jobs in the
  namespace only if they run the agent's own ECR image, as `sre-agent`, with
  the `investigate` entrypoint, one container, no host namespaces, only the
  MCP token volume, and the restricted security context.
- **Limits.** `backoffLimit: 0`, a one-hour deadline, and per-incident caps
  on tool calls, model calls and output tokens; at most one PR per
  incident.
- **Network.** Nothing may connect to a worker. Workers may reach DNS, the
  MCP server, the Pod Identity agent, and HTTPS outside the VPC (Bedrock,
  S3). Hostname filtering needs an egress proxy or VPC endpoints.

Running the agent in the same cluster it investigates is a PoC compromise.
The production design (a separate operations cluster and account,
PrivateLink, OIDC-federated identity) is in
[production-agent-topology.md](production-agent-topology.md).

## MCP server

The governed interface between the agent and the platform
(`apps/mcp-server`). It authenticates callers with Kubernetes TokenReview,
authorises every request with an embedded OPA policy
(`apps/mcp-server/policy/mcp.rego`, also served as `platform://policy/mcp`),
and logs every decision.

- **Tools:** read-only investigation tools for in-scope services, one write
  (`create_pull_request`, limited to `gitops/workloads/**` on `agent/*`
  branches, no Secrets), and one tool that always refuses (`patch_deployment`).
- **Resources:** documentation read live from the repository.
- **Prompts:** the investigate / remediate / verify workflows.
- Its own permissions: read-only RBAC without Secrets, no AWS role, and the
  GitHub App above.

## CI/CD

GitHub Actions authenticates to AWS only through OIDC; no static key is
stored anywhere. The provider and roles are created by
`scripts/bootstrap-ci.sh`, outside Terraform, so CI works while the
environment is down.

| Workflow | Trigger | Does |
|---|---|---|
| `ci.yml` | Every PR (the agent's included) | Go fmt/vet/test, Terraform fmt/validate/TFLint, Trivy, Checkov, Conftest, promtool, actionlint, shellcheck; owner-only `terraform plan` (+ Infracost) when `terraform/` changed |
| `images.yml` | Merge to `main` changing `apps/` | Build → Trivy scan → push to ECR → open a PR pinning the new tags |
| `environment.yml` | Manual, owner-only | `create-env.sh` (up) or `destroy-env.sh` (down) |

The delivery path for code is: PR → CI → human merge → image built,
scanned, pushed → pin PR → CI → human merge → Argo CD deploys. A human
merge gates both steps, and nothing in CI touches the cluster or pushes to
`main`.

### Known gap: no branch protection

The repository is private on GitHub Free, which offers no branch protection
or rulesets for private repositories. Consequences:

- The MCP server's GitHub App needs Contents: write to create `agent/*`
  branches, and GitHub can't scope that to branches, so the credential could
  technically push to `main`. What prevents it is the MCP server itself (OPA
  policy and code only ever create `agent/*` branches and open PRs) and the
  fact that the agent holds no GitHub credential. A compromised MCP server
  pod would not be stopped by GitHub.
- "CI passes and a human approves before merge" is a working agreement, not
  something GitHub enforces.
- GitHub Environments with required reviewers aren't available either, so
  privileged CI jobs (plan, image push, environment up/down) are gated by
  `scripts/ci/require-owner.sh`: they stop unless the repository owner
  caused the run. Anyone with write access could edit a workflow to drop
  the check; here only the owner has write access.

Closing it needs branch protection on `main`: a public repository, or GitHub
Pro. With it, require a pull request with one approval and passing status
checks, and don't allow bypass.

## Intentional failure demos

- A commit setting a wrong `DATABASE_HOST` on payment-api → HTTP 500s → alert.
- The agent's direct patch is denied by policy.
- An AWS call outside the agent's role returns `AccessDenied`.
- A manifest with `privileged: true` fails the OPA check in CI.
