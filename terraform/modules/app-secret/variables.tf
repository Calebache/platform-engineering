variable "name" {
  description = "Secrets Manager name. Keep it under <project>-<env>/ so the External Secrets role can read it."
  type        = string
}

variable "description" {
  type    = string
  default = ""
}

variable "username" {
  description = "Stored alongside the generated password as {username, password}."
  type        = string
}

variable "password_length" {
  type    = number
  default = 32
}

variable "recovery_window_in_days" {
  description = "0 deletes immediately on destroy, so the same name can be reused when the environment is recreated."
  type        = number
  default     = 0
}
