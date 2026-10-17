output "oidc_provider_arn" {
  value = local.oidc_provider_arn
}

output "role_arns" {
  value = { for key, role in aws_iam_role.this : key => role.arn }
}
