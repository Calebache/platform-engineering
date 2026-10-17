variable "name_prefix" {
  description = "Prefix for role names, e.g. agentic-sre-dev."
  type        = string
}

variable "create_oidc_provider" {
  description = "Create the account-wide GitHub OIDC provider. Set false if another environment already created it."
  type        = bool
  default     = true
}

variable "max_session_duration" {
  type    = number
  default = 3600
}

variable "roles" {
  description = <<-EOT
    Roles keyed by short name (the role is named <name_prefix>-<key>).
    subject: the exact GitHub OIDC `sub` claim to trust,
      e.g. repo:owner/name:environment:dev or repo:owner/name:pull_request.
  EOT
  type = map(object({
    subject             = string
    managed_policy_arns = optional(list(string), [])
    inline_policies     = optional(map(string), {})
  }))
}
