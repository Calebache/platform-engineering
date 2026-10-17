output "name" {
  value = aws_secretsmanager_secret.this.name
}

output "arn" {
  value = aws_secretsmanager_secret.this.arn
}
