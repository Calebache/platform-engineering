# dev: disposable PoC environment. Create before a demo, destroy after.
#   terraform init -backend-config=environments/dev.s3.tfbackend
#   terraform apply -var-file=environments/dev.tfvars

environment = "dev"
region      = "ca-central-1"
account_id  = "103544553431"
domain_name = "calebache.com"

# The agent's model. This account can't use Claude Opus 4.7+ (or Bedrock's
# Mantle Messages endpoint), so the agent uses Opus 4.6 through bedrock-runtime
# in the cluster's own region; the global profile picks where inference runs.
bedrock_region            = "ca-central-1"
bedrock_inference_profile = "global.anthropic.claude-opus-4-6-v1"
bedrock_foundation_model  = "anthropic.claude-opus-4-6-v1"

# Network: public + private subnet in each of two AZs (EKS control plane
# needs two). Nodes and the single NAT gateway stay in one AZ.
vpc_cidr                      = "10.0.0.0/16"
availability_zones            = ["ca-central-1a", "ca-central-1b"]
nat_gateway_availability_zone = "ca-central-1a"
node_availability_zones       = ["ca-central-1a"]

# EKS
kubernetes_version  = "1.36"
node_instance_types = ["t3.medium"]
node_min_size       = 2
node_desired_size   = 2
node_max_size       = 3
# With prefix delegation; AWS's recommended value for instances under 30 vCPUs.
# CPU and memory, not IP addresses, now limit how many pods fit.
node_max_pods = 110

# Open so GitHub-hosted runners can reach the API for the Argo CD bootstrap;
# access still requires an IAM identity with an EKS access entry. To lock
# down, set your public IP (`curl https://checkip.amazonaws.com`) as /32.
cluster_endpoint_public_access_cidrs = ["0.0.0.0/0"]
# Cluster admins besides the CI deployer role (always added). Whoever runs
# `terraform apply` gets no implicit access, so the same admins exist whether
# you or CI created the cluster. Update this if your SSO permission set is
# re-provisioned (its role name changes).
cluster_admin_principal_arns = [
  "arn:aws:iam::103544553431:role/aws-reserved/sso.amazonaws.com/us-east-2/AWSReservedSSO_AdministratorAccess_43d3f2124756555b",
]

# ECR repositories and the audit bucket outlive the environment: created by
# scripts/bootstrap-artifacts.sh, only looked up here.
ecr_repositories = ["mcp-server", "sre-agent", "payment-api"]

# Argo CD: root app syncs gitops/apps/dev from this branch.
gitops_target_revision = "main"
