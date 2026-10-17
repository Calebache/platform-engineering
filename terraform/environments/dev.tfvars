# dev: disposable PoC environment. Create before a demo, destroy after.
#   terraform init -backend-config=environments/dev.s3.tfbackend
#   terraform apply -var-file=environments/dev.tfvars

environment       = "dev"
region            = "ca-central-1"
account_id        = "103544553431"
state_bucket_name = "calebache-platform-tfstate-103544553431"
domain_name       = "calebache.com"

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
cluster_admin_principal_arns         = []

# ECR
ecr_repositories = ["mcp-server", "sre-agent", "payment-api"]
ecr_force_delete = true

# Audit
audit_bucket_force_destroy = true

# Argo CD: root app syncs gitops/apps/dev from this branch.
gitops_target_revision = "main"

# First (and only) environment in this account creates the GitHub OIDC provider.
create_github_oidc_provider = true
