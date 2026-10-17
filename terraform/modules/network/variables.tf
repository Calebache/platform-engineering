variable "name" {
  description = "Name prefix for network resources."
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster name, used for the subnet discovery tag."
  type        = string
}

variable "vpc_cidr" {
  type = string
}

variable "availability_zones" {
  description = "One public and one private subnet are created per AZ, in this order."
  type        = list(string)
}

variable "nat_gateway_availability_zone" {
  description = "AZ whose public subnet hosts the single NAT gateway. Put it where the nodes run."
  type        = string

  validation {
    condition     = contains(var.availability_zones, var.nat_gateway_availability_zone)
    error_message = "nat_gateway_availability_zone must be one of availability_zones."
  }
}

variable "subnet_newbits" {
  description = "Bits added to the VPC prefix for each subnet (/16 + 4 = /20)."
  type        = number
  default     = 4
}

variable "private_subnet_offset" {
  description = "Subnet index where private subnets start, leaving room for more public subnets below it."
  type        = number
  default     = 8
}
