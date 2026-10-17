variable "name" {
  description = "IAM role name."
  type        = string
}

variable "account_id" {
  type = string
}

variable "cluster_name" {
  type = string
}

variable "cluster_arn" {
  type = string
}

variable "namespace" {
  type = string
}

variable "service_account" {
  type = string
}

variable "policy_json" {
  description = "Inline permissions policy for the role."
  type        = string
}
