output "region" {
  value = var.region
}

output "cluster_name" {
  value = module.eks.cluster_name
}

output "cluster_endpoint" {
  value = module.eks.cluster_endpoint
}

output "kubeconfig_command" {
  value = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name}"
}

# Pass to the agent as its Bedrock client region (not AWS_REGION).
output "bedrock_region" {
  value = var.bedrock_region
}

output "vpc_id" {
  value = module.network.vpc_id
}

output "nat_gateway_public_ip" {
  description = "Egress IP for the cluster; allow-list it where outbound traffic needs to be recognised."
  value       = module.network.nat_gateway_public_ip
}

output "ecr_repository_urls" {
  value = module.ecr.repository_urls
}

output "audit_bucket_name" {
  value = module.audit_bucket.bucket_name
}

output "route53_zone_id" {
  value = data.aws_route53_zone.main.zone_id
}

# Set these as repository/environment variables in GitHub Actions.
output "github_role_arns" {
  value = module.github_oidc.role_arns
}

output "workload_role_arns" {
  value = { for name, identity in module.workload_identity : name => identity.role_arn }
}
