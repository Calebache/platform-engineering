terraform {
  # 1.11+ for write-only attributes (the Argo CD repo key stays out of state).
  required_version = ">= 1.11.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 3.0"
    }
  }

  # Partial config: each environment supplies its own state key through
  # environments/<env>.s3.tfbackend. S3 native locking (use_lockfile) needs
  # no DynamoDB table.
  backend "s3" {}
}

provider "aws" {
  region              = var.region
  allowed_account_ids = [var.account_id]

  default_tags {
    tags = {
      Project     = var.project
      Environment = var.environment
      ManagedBy   = "terraform"
      Repo        = var.github_repo
    }
  }
}

# The Kubernetes-side providers are configured from the cluster this same
# apply creates, which is what makes `terraform apply` a single step. Tokens
# come from `aws eks get-token` at call time (no 15-minute expiry mid-apply),
# so the machine running Terraform needs the AWS CLI and the same credentials.
locals {
  eks_exec_auth = {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--cluster-name", module.eks.cluster_name, "--region", var.region]
  }
}

provider "kubernetes" {
  host                   = module.eks.cluster_endpoint
  cluster_ca_certificate = base64decode(module.eks.cluster_certificate_authority_data)

  exec {
    api_version = local.eks_exec_auth.api_version
    command     = local.eks_exec_auth.command
    args        = local.eks_exec_auth.args
  }
}

provider "helm" {
  kubernetes = {
    host                   = module.eks.cluster_endpoint
    cluster_ca_certificate = base64decode(module.eks.cluster_certificate_authority_data)
    exec                   = local.eks_exec_auth
  }
}
