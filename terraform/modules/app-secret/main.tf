# A generated credential in Secrets Manager that Terraform never knows.
# The password is an ephemeral value written through a write-only attribute,
# so it appears in neither state nor plan. External Secrets syncs it into the
# cluster.
#
# Terraform only seeds the first value when the secret is created. Rotation
# happens in Secrets Manager directly; Terraform never rewrites the value
# (the write-only version is fixed), so it won't revert a rotated password.

ephemeral "random_password" "this" {
  length  = var.password_length
  special = false
}

resource "aws_secretsmanager_secret" "this" {
  name        = var.name
  description = var.description

  # Disposable environments are recreated with the same secret names; the
  # default 30-day recovery window would block that.
  recovery_window_in_days = var.recovery_window_in_days
}

resource "aws_secretsmanager_secret_version" "this" {
  secret_id = aws_secretsmanager_secret.this.id
  secret_string_wo = jsonencode({
    username = var.username
    password = ephemeral.random_password.this.result
  })
  secret_string_wo_version = 1
}
