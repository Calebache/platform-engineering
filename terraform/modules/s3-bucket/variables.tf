variable "bucket_name" {
  description = "Globally unique bucket name."
  type        = string
}

variable "force_destroy" {
  description = "Allow `terraform destroy` to delete a non-empty bucket (disposable environments)."
  type        = bool
  default     = false
}

variable "versioning_enabled" {
  type    = bool
  default = true
}

variable "noncurrent_version_expiration_days" {
  description = "Expire old object versions after this many days. null keeps them forever."
  type        = number
  default     = null
}
