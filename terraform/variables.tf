# Environment-specific values have no default and must come from
# environments/<env>.tfvars.

# --- Account and naming ---------------------------------------------------

variable "project" {
  description = "Project name; prefixes resource names."
  type        = string
  default     = "agentic-sre"
}

variable "environment" {
  description = "Environment name. Also the GitHub Environment the deployer role trusts."
  type        = string
}

variable "region" {
  type = string
}

variable "account_id" {
  description = "AWS account this environment is allowed to run against."
  type        = string
}

variable "github_repo" {
  description = "GitHub repository (owner/name) trusted by the OIDC roles."
  type        = string
  default     = "Calebache/platform-engineering"
}

variable "state_bucket_name" {
  description = "Terraform state bucket created by scripts/bootstrap-state-bucket.sh. The plan role needs it for the state lock."
  type        = string
}

variable "bedrock_region" {
  description = "Region of the Bedrock Mantle endpoint the agent calls. Can differ from var.region when Mantle isn't offered there."
  type        = string
}

variable "domain_name" {
  description = "Public Route53 hosted zone managed by external-dns and cert-manager."
  type        = string
}

# --- Network --------------------------------------------------------------

variable "vpc_cidr" {
  type = string
}

variable "availability_zones" {
  description = "AZs for public subnets. EKS requires control-plane subnets in at least two."
  type        = list(string)

  validation {
    condition     = length(var.availability_zones) >= 2
    error_message = "EKS needs subnets in at least two availability zones."
  }
}

variable "nat_gateway_availability_zone" {
  description = "AZ hosting the single NAT gateway. Match node_availability_zones to avoid cross-AZ data charges."
  type        = string
}

variable "node_availability_zones" {
  description = "Subset of availability_zones that worker nodes run in."
  type        = list(string)

  validation {
    condition     = length(setsubtract(var.node_availability_zones, var.availability_zones)) == 0
    error_message = "node_availability_zones must be a subset of availability_zones."
  }
}

# --- EKS ------------------------------------------------------------------

variable "kubernetes_version" {
  description = "Check `aws eks describe-cluster-versions` for versions in standard support."
  type        = string
}

variable "node_instance_types" {
  type = list(string)
}

variable "node_min_size" {
  type = number
}

variable "node_desired_size" {
  type = number
}

variable "node_max_size" {
  type = number
}

variable "cluster_endpoint_public_access_cidrs" {
  description = "CIDRs allowed to reach the public EKS API endpoint."
  type        = list(string)
}

variable "cluster_admin_principal_arns" {
  description = "Extra IAM principals (e.g. your SSO role) granted cluster-admin. The deployer role is always added."
  type        = list(string)
  default     = []
}

# --- ECR ------------------------------------------------------------------

variable "ecr_repositories" {
  description = "ECR repository names. Names are account-wide, so environments in one account must not overlap."
  type        = list(string)
}

variable "ecr_force_delete" {
  type    = bool
  default = false
}

# --- Audit ----------------------------------------------------------------

variable "audit_bucket_force_destroy" {
  description = "Let `terraform destroy` delete the audit bucket with records in it."
  type        = bool
  default     = false
}

# --- Argo CD --------------------------------------------------------------

variable "argocd_chart_version" {
  description = "argo-cd Helm chart version. 10.9.6 = Argo CD v3.5.3."
  type        = string
  default     = "10.9.6"
}

variable "argocd_apps_chart_version" {
  type    = string
  default = "2.0.6"
}

variable "argocd_repo_secret_name" {
  description = "Secrets Manager secret holding Argo CD's read-only GitHub App credentials (scripts/store-argocd-github-app.sh)."
  type        = string
  default     = "agentic-sre/argocd/github-app"
}

variable "argocd_repo_secret_revision" {
  description = "Increment after rotating the GitHub App key so Terraform re-writes it."
  type        = number
  default     = 1
}

variable "gitops_target_revision" {
  description = "Branch the root Application tracks; it syncs gitops/apps/<environment>."
  type        = string
}

# --- GitHub OIDC ----------------------------------------------------------

variable "create_github_oidc_provider" {
  description = "Only one GitHub OIDC provider can exist per account; set false for every environment after the first."
  type        = bool
  default     = true
}
