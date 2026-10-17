locals {
  name         = "${var.project}-${var.environment}"
  cluster_name = local.name
}

module "network" {
  source = "./modules/network"

  name                          = local.name
  cluster_name                  = local.cluster_name
  vpc_cidr                      = var.vpc_cidr
  availability_zones            = var.availability_zones
  nat_gateway_availability_zone = var.nat_gateway_availability_zone
}

module "eks" {
  source = "./modules/eks"

  cluster_name       = local.cluster_name
  kubernetes_version = var.kubernetes_version

  # Control-plane ENIs and nodes live in private subnets only.
  control_plane_subnet_ids     = module.network.private_subnet_ids
  node_subnet_ids              = [for az in var.node_availability_zones : module.network.private_subnet_ids_by_az[az]]
  endpoint_public_access_cidrs = var.cluster_endpoint_public_access_cidrs

  node_instance_types = var.node_instance_types
  node_min_size       = var.node_min_size
  node_desired_size   = var.node_desired_size
  node_max_size       = var.node_max_size
  node_max_pods       = var.node_max_pods

  cluster_admins = merge(
    { "terraform-deployer" = module.github_oidc.role_arns["terraform-deployer-role"] },
    { for idx, arn in var.cluster_admin_principal_arns : "extra-${idx}" => arn },
  )
}

# --- Long-lived artifacts -------------------------------------------------
# ECR repositories (images) and the audit bucket outlive every environment,
# so they're created by scripts/bootstrap-artifacts.sh, not here. Terraform
# only looks them up; a missing one fails the plan with a clear error.

data "aws_ecr_repository" "app" {
  for_each = toset(var.ecr_repositories)
  name     = each.value
}

# One JSON record per agent execution (doc.md §16).
data "aws_s3_bucket" "audit" {
  bucket = "${local.name}-audit-${var.account_id}"
}

# These used to be managed here. Hand them over to the bootstrap script
# without deleting them (and their images and records). No-op on a fresh
# environment.
removed {
  from = module.ecr
  lifecycle {
    destroy = false
  }
}

removed {
  from = module.audit_bucket
  lifecycle {
    destroy = false
  }
}

module "github_oidc" {
  source = "./modules/github-oidc"

  name_prefix          = local.name
  create_oidc_provider = var.create_github_oidc_provider

  roles = {
    # `terraform apply`: only jobs bound to the GitHub Environment, which is
    # where the human approval gate lives (configure required reviewers there).
    #
    # Deliberate exception to least privilege: this role creates IAM, EKS and
    # VPC resources, so it is effectively admin. It is contained by the trust
    # policy and never usable by workloads. Tightening it is follow-up work.
    "terraform-deployer-role" = {
      subject             = "repo:${var.github_repo}:environment:${var.environment}"
      managed_policy_arns = ["arn:aws:iam::aws:policy/AdministratorAccess"]
    }

    # `terraform plan` on pull requests.
    "terraform-plan-role" = {
      subject             = "repo:${var.github_repo}:pull_request"
      managed_policy_arns = ["arn:aws:iam::aws:policy/ReadOnlyAccess"]
      inline_policies     = { "state-lock" = data.aws_iam_policy_document.plan_state_lock.json }
    }

    # Image pushes happen only from main.
    "image-builder-role" = {
      subject         = "repo:${var.github_repo}:ref:refs/heads/main"
      inline_policies = { "ecr-push" = data.aws_iam_policy_document.image_builder.json }
    }
  }
}

# Read-only GitHub App credentials stored by scripts/store-github-app.sh (PROFILE=argocd).
# Ephemeral: read at plan/apply time, never written to state or the plan file.
ephemeral "aws_secretsmanager_secret_version" "argocd_github_app" {
  secret_id = var.argocd_repo_secret_name
}

locals {
  argocd_github_app = jsondecode(ephemeral.aws_secretsmanager_secret_version.argocd_github_app.secret_string)
}

# GitOps bootstrap: the last thing Terraform does inside Kubernetes. Waits for
# the whole EKS module (nodes, CoreDNS, access entries) before installing.
module "argocd" {
  source = "./modules/argocd"

  chart_version        = var.argocd_chart_version
  apps_chart_version   = var.argocd_apps_chart_version
  repo_name            = var.github_repo
  repo_url             = "https://github.com/${var.github_repo}.git"
  repo_secret_revision = var.argocd_repo_secret_revision

  github_app = {
    app_id          = local.argocd_github_app.appId
    installation_id = local.argocd_github_app.installationId
    private_key     = local.argocd_github_app.privateKey
  }
  target_revision = var.gitops_target_revision
  root_app_path   = "gitops/apps/${var.environment}"

  depends_on = [module.eks]
}

# Workload identities via EKS Pod Identity: each role is bound to one
# namespace/ServiceAccount and carries only the policy in policies.tf.
module "workload_identity" {
  source   = "./modules/pod-identity-role"
  for_each = local.workload_identities

  name            = "${local.name}-${each.key}-role"
  account_id      = var.account_id
  cluster_name    = module.eks.cluster_name
  cluster_arn     = module.eks.cluster_arn
  namespace       = each.value.namespace
  service_account = each.value.service_account
  policy_json     = each.value.policy_json
}

locals {
  workload_identities = {
    "sre-agent" = {
      namespace       = "sre-agent"
      service_account = "sre-agent"
      policy_json     = data.aws_iam_policy_document.sre_agent.json
    }
    "external-dns" = {
      namespace       = "external-dns"
      service_account = "external-dns"
      policy_json     = data.aws_iam_policy_document.external_dns.json
    }
    "cert-manager" = {
      namespace       = "cert-manager"
      service_account = "cert-manager"
      policy_json     = data.aws_iam_policy_document.cert_manager.json
    }
    "cluster-autoscaler" = {
      namespace       = "kube-system"
      service_account = "cluster-autoscaler"
      policy_json     = data.aws_iam_policy_document.cluster_autoscaler.json
    }
    # Provisions the NLB in front of Envoy Gateway. Upstream policy, vendored
    # unmodified from the controller's v3.5.0 release (docs/install/iam_policy.json).
    "aws-load-balancer-controller" = {
      namespace       = "kube-system"
      service_account = "aws-load-balancer-controller"
      policy_json     = jsonencode(jsondecode(file("${path.module}/iam-policies/aws-load-balancer-controller.json")))
    }
    # Syncs Secrets Manager entries under <project>-<env>/ into Kubernetes
    # Secrets. Can't read anything outside that prefix (e.g. Argo CD's
    # GitHub App credentials).
    "external-secrets" = {
      namespace       = "external-secrets"
      service_account = "external-secrets"
      policy_json     = data.aws_iam_policy_document.external_secrets.json
    }
  }
}

# --- Application secrets --------------------------------------------------
# Generated credentials that only ever exist in Secrets Manager and in the
# cluster (via External Secrets), never in Git or Terraform state.

module "payment_api_db_secret" {
  source = "./modules/app-secret"

  name        = "${local.name}/payment-api/db"
  description = "payment-api Postgres credentials"
  username    = "payments"
}
