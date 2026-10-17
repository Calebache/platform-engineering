variable "cluster_name" {
  type = string
}

variable "kubernetes_version" {
  description = "Check `aws eks describe-cluster-versions` for versions in standard support."
  type        = string
}

variable "control_plane_subnet_ids" {
  description = "Subnets for control-plane ENIs. EKS requires at least two AZs."
  type        = list(string)
}

variable "node_subnet_ids" {
  description = "Subnets the managed node group launches into."
  type        = list(string)
}

variable "endpoint_public_access_cidrs" {
  description = "CIDRs allowed to reach the public API endpoint."
  type        = list(string)
}

variable "enabled_log_types" {
  description = "Control-plane log types sent to CloudWatch."
  type        = list(string)
  default     = ["audit", "authenticator"]
}

variable "log_retention_days" {
  type    = number
  default = 7
}

variable "node_instance_types" {
  type = list(string)
}

variable "node_ami_type" {
  type    = string
  default = "AL2023_x86_64_STANDARD"
}

variable "node_capacity_type" {
  type    = string
  default = "ON_DEMAND"
}

variable "node_disk_size" {
  description = "Node root volume size in GiB."
  type        = number
  default     = 20
}

variable "node_max_pods" {
  description = "Kubelet maxPods. With VPC CNI prefix delegation, AWS recommends 110 for instances under 30 vCPUs."
  type        = number
}

variable "node_min_size" {
  type = number
}

variable "node_desired_size" {
  type = number
}

variable "node_max_size" {
  type = number
}

variable "cluster_admins" {
  description = <<-EOT
    IAM principals granted cluster-admin through access entries, keyed by a
    static label so for_each works while ARNs are still unknown at plan time.
  EOT
  type        = map(string)
  default     = {}
}
