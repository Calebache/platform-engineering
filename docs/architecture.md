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
| Agent model | Claude Opus 5.5 on Amazon Bedrock, called in `us-east-1` |

The environment is disposable: created with one `terraform apply`, removed
with `scripts/destroy-env.sh dev`.

## Ownership boundaries

| Layer | Owner | Contents |
|---|---|---|
| Cloud substrate | Terraform (`terraform/`) | VPC, EKS, ECR, IAM, GitHub OIDC, Pod Identity, Secrets Manager entries, audit bucket |
| GitOps bootstrap | Terraform `argocd` module | Argo CD and one root Application, nothing more |
| Everything in the cluster | Argo CD (`gitops/`) | Platform controllers, config, workloads |
| Out-of-band, outlives environments | Scripts (`scripts/`) | Terraform state bucket, GitHub App credentials |

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
| GitHub Actions | OIDC → STS, per-duty roles | `terraform-deployer` (env-gated), `terraform-plan`, `image-builder` |
| Workloads | EKS Pod Identity, one role per ServiceAccount | Only their own AWS API calls |
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
| -2 | Envoy Gateway, kube-prometheus-stack, cluster-autoscaler | Gateway API, monitoring CRDs, node scaling |
| -1 | cert-manager, external-dns, External Secrets | TLS, DNS records, secrets |
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
Alertmanager ──webhook──> SRE agent ──MCP──> MCP server ──> Kubernetes (read)
                              │                        ├──> Prometheus (named queries)
                              │                        ├──> Argo CD (read)
                              │                        └──> GitHub (read, PRs)
                              └──> Bedrock (Claude), S3 audit record
```

1. An alert fires and reaches the agent.
2. The agent investigates only through MCP tools, starting with
   `get_incident_context`, and reads the service runbook resource.
3. It forms a hypothesis with evidence (log patterns, deploy history, commits).
4. Direct mutation is refused by policy (`patch_deployment` →
   `required_path: create_pull_request`).
5. It opens a PR; CI validates it; a human approves and merges.
6. Argo CD deploys the fix; the agent verifies recovery with metrics.
7. The agent writes an audit record to S3.

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

Closing it needs branch protection on `main`: a public repository, or GitHub
Pro. With it, require a pull request with one approval and passing status
checks, and don't allow bypass.

## Intentional failure demos

- A commit setting a wrong `DATABASE_HOST` on payment-api → HTTP 500s → alert.
- The agent's direct patch is denied by policy.
- An AWS call outside the agent's role returns `AccessDenied`.
- A manifest with `privileged: true` fails the OPA check in CI.
