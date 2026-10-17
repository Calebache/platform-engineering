# Agentic SRE Platform PoC — Implementation Summary

## Objective

Build a small but production-minded Agentic SRE platform on AWS that demonstrates how AI agents can safely investigate incidents, propose remediation, and operate through governed interfaces without bypassing existing DevOps, security, and platform controls.

The PoC should demonstrate the full lifecycle:

**Provision → Deploy → Observe → Detect → Investigate → Propose Fix → Validate → Approve → Deploy → Verify → Audit**

The goal is not to build a fully autonomous production platform. The goal is to demonstrate how existing platform engineering principles can safely support agentic workloads.

---

## Core Design Principles

The platform will follow established DevOps and cloud architecture principles:

- Least privilege
- Deny by default
- Short-lived credentials
- Federated identity
- Workload-specific IAM roles
- Kubernetes RBAC and namespace isolation
- Immutable infrastructure where practical
- Infrastructure as Code
- GitOps-based deployment
- Deterministic validation gates
- Human approval for high-risk actions
- Full auditability
- Disposable environments
- Separation of platform capabilities from specific tools

AI agents are treated as workloads, not as trusted administrators.

The agent may reason probabilistically, but all consequential actions must pass through deterministic security and validation controls.

---

# 1. AWS Infrastructure

Terraform will provision the AWS infrastructure.

Initial PoC configuration:

- Single AWS region
- Single Availability Zone
- One VPC
- Private subnets for the EKS worker nodes and control-plane network interfaces
- Public subnets only for internet-facing load balancers and the NAT Gateway
- Internet Gateway
- One NAT Gateway (single AZ, matching the worker nodes) for outbound traffic from private subnets
- S3 gateway VPC endpoint (free) so S3 traffic bypasses the NAT Gateway
- EKS cluster
- Two low-cost EC2 worker nodes
- Amazon ECR repositories
- IAM roles and policies
- GitHub Actions OIDC federation
- EKS workload identities
- Terraform remote state in S3
- Terraform state locking using the S3 backend
- Minimal CloudWatch integration where useful

The environment will intentionally remain small and inexpensive because it is designed to be created before testing or an interview and destroyed afterward.

Worker nodes have no public IP addresses and cannot be reached from the internet; all inbound traffic enters through load balancers in the public subnets.

Production improvements such as multi-AZ worker nodes, one NAT Gateway per AZ, interface VPC endpoints, and highly available platform services will be documented but not required for the PoC.

---

# 2. Terraform Ownership

Terraform owns the cloud substrate.

Terraform will manage:

- VPC
- Subnets
- Internet Gateway
- NAT Gateway
- VPC endpoints
- Security Groups
- EKS
- EKS Node Groups
- ECR
- IAM
- GitHub OIDC integration
- Pod Identity / workload IAM
- Route53 permissions where required
- Terraform backend
- Argo CD bootstrap

Terraform will not permanently manage Kubernetes platform applications.

Its Kubernetes responsibility ends after Argo CD is installed and given its initial root application.

---

# 3. GitOps Bootstrap

The desired workflow is:

```text
terraform apply
      |
      +--> AWS Infrastructure
      |
      +--> EKS
      |
      +--> Argo CD
              |
              +--> Root Application
                       |
                       +--> ingress-nginx
                       +--> cert-manager
                       +--> external-dns
                       +--> policy components
                       +--> observability
                       +--> MCP server
                       +--> agent runtime
                       +--> demo application
```

This provides one-command environment creation without making Terraform the permanent owner of Kubernetes workloads.

Terraform handles **bootstrap**.

Argo CD handles **steady-state reconciliation**.

---

# 4. Argo CD

Argo CD becomes the source of truth for Kubernetes platform and application resources after bootstrap.

It will manage:

- ingress-nginx
- cert-manager
- external-dns
- policy components
- observability components
- MCP server
- Agent runtime
- Demo application
- Supporting Kubernetes resources

Argo CD Applications or an App-of-Apps pattern will be used.

Deployment ordering will be controlled using separate applications and/or Argo CD sync waves.

Example:

```text
Wave -2
Namespaces / CRDs

Wave -1
cert-manager
ingress-nginx
external-dns

Wave 0
ClusterIssuer
OPA policies
platform configuration

Wave 1
MCP server
agent runtime
demo applications

Wave 2
Ingress resources
```

---

# 5. GitHub Actions and OIDC

GitHub Actions will be used for CI/CD.

No static AWS credentials will be stored in GitHub.

GitHub Actions will authenticate to AWS through:

```text
GitHub Actions
      |
      | OIDC
      v
AWS STS
      |
      | AssumeRoleWithWebIdentity
      v
Scoped temporary IAM credentials
```

OIDC trust policies will be restricted to the appropriate:

- GitHub organization
- Repository
- Branch
- Environment

Separate IAM roles will be created for separate responsibilities.

Examples:

```text
terraform-deployer-role
image-builder-role
agent-runtime-role
external-dns-role
```

The Terraform deployment identity will not be reused by application or agent workloads.

---

# 6. Container Images

Amazon ECR will store all container images.

Initial repositories may include:

```text
mcp-server
sre-agent
demo-app
```

Images will be:

- Built by GitHub Actions
- Scanned for vulnerabilities
- Tagged using Git commit SHA
- Preferably configured with immutable tags
- Avoided using `latest`
- Deployed through Argo CD

---

# 7. CI Validation Pipeline

Pull requests will run deterministic validation before any deployment.

Example pipeline:

```text
Pull Request
      |
      +--> formatting
      +--> linting
      +--> unit tests
      +--> terraform validate
      +--> terraform plan
      +--> security scanning
      +--> OPA / policy checks
      +--> cost analysis
      +--> container scanning
```

Possible tooling:

- Terraform fmt / validate
- TFLint
- Trivy
- Checkov or equivalent
- OPA / Conftest
- Infracost
- Application unit/integration tests

Production or privileged changes will require an approval gate.

The agent cannot override these controls.

---

# 8. Agent Runtime

The SRE agent will run as a Kubernetes workload.

Different workloads may use different execution models:

- Long-running orchestrator service for task state and workflow coordination
- Kubernetes Jobs for bounded agent tasks
- CronJobs for scheduled operations
- Ephemeral Jobs for isolated code execution or investigations

The agent receives only the permissions required for its assigned function.

Initially, its permissions should be primarily read-only.

For example:

Allowed:

```text
read Kubernetes state
read logs
read metrics
read deployment history
read Git information
create proposed changes
create pull requests
```

Not allowed:

```text
modify IAM
delete infrastructure
directly patch production deployments
push directly to main
bypass GitOps
```

---

# 9. MCP / Tool Gateway

The MCP server acts as the governed interface between AI agents and operational systems.

Example capabilities:

```text
get_service_health()
get_recent_logs()
get_error_patterns()
get_recent_deployments()
get_git_changes()
get_metrics()
get_incident_context()
create_pull_request()
```

The MCP server may perform lightweight context engineering, including:

- Filtering
- Aggregation
- Deduplication
- Correlation
- Time-window selection
- Representative sample selection

The MCP layer will not rely on the LLM to determine security permissions.

Authorization is enforced independently.

---

# 10. Context Engineering

The system should avoid returning large amounts of redundant operational data to the model.

Example:

Instead of returning:

```text
50,000 raw log lines
```

the MCP service may return:

```text
1,200 errors detected

Error group A: 950 occurrences
Error group B: 230 occurrences
Error group C: 20 occurrences

Representative stack traces
First/last timestamps
Trace IDs
Deployment correlation
Relevant commit references
```

The agent can request deeper evidence if required.

This provides progressive disclosure:

```text
Summary
   ↓
Structured evidence
   ↓
Detailed evidence
   ↓
Raw source data
```

Raw evidence remains accessible so that context filtering does not become an irreversible information-loss layer.

---

# 11. Demo Application

A simple application will run inside EKS.

It should expose enough functionality to demonstrate realistic failures.

Example application:

```text
payment-api
```

Possible deliberate failure:

```text
DATABASE_HOST configured incorrectly
```

This results in HTTP 500 responses.

Prometheus detects the elevated failure rate and triggers an incident workflow.

---

# 12. Observability

The platform should provide enough observability for the agent to investigate real evidence.

Initial stack:

- Prometheus
- Grafana
- Kubernetes logs
- Optional Loki if required

The system should expose:

- HTTP request rates
- HTTP error rates
- Application health
- Kubernetes health
- Deployment changes
- Agent tool calls
- Agent execution history

The PoC does not require a large observability stack.

---

# 13. Incident Investigation Flow

The primary demo scenario is:

```text
Application develops HTTP 500 errors
           |
           v
Monitoring detects elevated error rate
           |
           v
Agent receives incident
           |
           v
MCP: get_service_health()
           |
           v
MCP: get_recent_errors()
           |
           v
MCP: get_recent_deployments()
           |
           v
MCP: get_git_changes()
           |
           v
Agent correlates evidence
           |
           v
Agent generates hypothesis
```

Example hypothesis:

```text
The failure began immediately after deployment X.

Logs show database connection failures.

Commit abc123 changed DATABASE_HOST.

Most likely cause:
invalid database endpoint introduced by commit abc123.
```

---

# 14. Governed Remediation

The agent may initially attempt an unsafe action:

```text
patch production deployment
```

The platform must reject it.

Example:

```json
{
  "allowed": false,
  "reason": "Direct production mutation is prohibited.",
  "required_path": "create_pull_request"
}
```

The agent must then follow the approved workflow:

```text
Agent
  |
  v
Generate change
  |
  v
Open Pull Request
  |
  v
GitHub Actions
  |
  +--> Tests
  +--> OPA
  +--> Security
  +--> Cost checks
  |
  v
Human approval
  |
  v
Merge
  |
  v
Argo CD
  |
  v
EKS
```

This demonstrates that model reasoning is bounded by deterministic platform controls.

---

# 15. Post-Deployment Verification

Deployment completion does not automatically mean incident resolution.

After Argo CD deploys the fix, the agent will verify:

- Application health
- HTTP 5xx rate
- Kubernetes readiness
- Error log frequency
- Relevant metrics
- Deployment status

Only after verification should the incident be considered resolved.

---

# 16. Agent Audit Trail

Each agent execution will generate an audit record.

Example:

```json
{
  "execution_id": "exec-001",
  "agent": "sre-agent",
  "initiated_by": "monitoring-alert",
  "service": "payment-api",
  "environment": "dev",
  "tools_used": [
    "get_service_health",
    "get_recent_logs",
    "get_recent_deployments",
    "get_git_changes"
  ],
  "hypothesis": "Database endpoint regression",
  "evidence": [
    "deployment:payment-api-v18",
    "commit:abc123",
    "trace:xyz"
  ],
  "proposed_action": "PR #27",
  "policy_result": "PASS",
  "security_result": "PASS",
  "human_approval": true,
  "deployment_result": "SUCCESS",
  "service_health": "RECOVERED"
}
```

The audit trail should answer:

> What did the agent do, why did it do it, what evidence did it use, what controls were evaluated, who approved it, and what was the final outcome?

---

# 17. IAM and Identity Demonstration

The PoC should explicitly demonstrate scoped identities.

Example agent:

```text
sre-agent ServiceAccount
       |
       v
EKS Pod Identity
       |
       v
sre-agent-role
       |
       +--> permitted resources
       |
       X--> prohibited resources
```

During the demo, the agent or Pod can run:

```bash
aws sts get-caller-identity
```

to prove which AWS identity it is using.

A permitted operation should succeed.

An unauthorized operation should return:

```text
AccessDenied
```

This demonstrates that the agent cannot exceed its assigned blast radius.

---

# 18. Security Demonstration

The PoC should intentionally include security failures.

Examples:

### Attempt 1

Agent tries to modify production directly.

Result:

```text
DENIED
```

### Attempt 2

Agent attempts to access an AWS resource outside its IAM scope.

Result:

```text
AccessDenied
```

### Attempt 3

A proposed manifest violates policy.

Example:

```yaml
securityContext:
  privileged: true
```

Result:

```text
OPA policy check failed
```

These failures are important because they demonstrate that the security model does not depend on the agent behaving correctly.

---

# 19. Infrastructure Lifecycle

The environment must be disposable.

Creation:

```bash
terraform apply
```

Terraform creates the infrastructure and bootstraps Argo CD.

Argo CD reconciles the remaining platform.

Destruction:

```bash
terraform destroy
```

The AWS infrastructure is removed when it is no longer required.

The local Dell server remains a development environment only and is not exposed publicly.

---

# 20. PoC Success Criteria

The PoC is complete when the following can be demonstrated end-to-end:

1. AWS infrastructure is created entirely through Terraform.
2. GitHub Actions authenticates to AWS through OIDC.
3. No static AWS access keys are used.
4. EKS workloads receive scoped workload identities.
5. Argo CD bootstraps the Kubernetes platform.
6. Application images are built and stored in ECR.
7. CI runs deterministic validation and policy checks.
8. A test application is deliberately broken.
9. Monitoring detects the failure.
10. The agent investigates using MCP tools.
11. The agent correlates logs, metrics, deployments, and code changes.
12. The agent proposes a remediation.
13. An unsafe direct action is denied.
14. The agent creates a PR instead.
15. CI validates the change.
16. Human approval is required for deployment.
17. Argo CD deploys the approved change.
18. The agent verifies that the service has recovered.
19. The full execution is recorded in an audit trail.
20. The complete AWS environment can be destroyed and recreated.

---

# Final Architecture Principle

The PoC is not intended to demonstrate that AI agents should replace platform engineering controls.

It demonstrates the opposite:

> AI agents become useful in production when they operate inside a well-designed platform that provides identity, isolation, policy, deterministic validation, observability, auditability, and controlled deployment paths.

MCP provides a governed and discoverable interface to platform capabilities.

Terraform creates the cloud substrate.

GitHub Actions validates changes.

OPA enforces policy.

Argo CD controls Kubernetes deployment.

AWS IAM and Kubernetes RBAC constrain identity and blast radius.

Observability provides evidence.

The agent provides reasoning and automation.

The platform remains the control system.