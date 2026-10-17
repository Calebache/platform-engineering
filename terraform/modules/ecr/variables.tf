variable "repositories" {
  type = list(string)
}

variable "image_tag_mutability" {
  type    = string
  default = "IMMUTABLE"
}

variable "force_delete" {
  description = "Allow deleting repositories that still contain images (disposable environments)."
  type        = bool
  default     = false
}

variable "untagged_expiry_days" {
  type    = number
  default = 7
}

variable "max_image_count" {
  type    = number
  default = 30
}
