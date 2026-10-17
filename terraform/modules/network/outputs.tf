output "vpc_id" {
  value = aws_vpc.this.id
}

output "public_subnet_ids" {
  description = "Public subnet IDs (load balancers, NAT), in availability_zones order."
  value       = [for az in var.availability_zones : aws_subnet.public[az].id]
}

output "private_subnet_ids" {
  description = "Private subnet IDs (EKS nodes and control plane), in availability_zones order."
  value       = [for az in var.availability_zones : aws_subnet.private[az].id]
}

output "private_subnet_ids_by_az" {
  value = { for az, subnet in aws_subnet.private : az => subnet.id }
}

output "nat_gateway_public_ip" {
  description = "Fixed egress IP for everything in the private subnets; usable for allow-listing."
  value       = aws_eip.nat.public_ip
}
