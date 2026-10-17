data "aws_iam_policy_document" "cluster_assume" {
  statement {
    actions = ["sts:AssumeRole", "sts:TagSession"]

    principals {
      type        = "Service"
      identifiers = ["eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "cluster" {
  name               = "${var.cluster_name}-cluster-role"
  assume_role_policy = data.aws_iam_policy_document.cluster_assume.json
}

resource "aws_iam_role_policy_attachment" "cluster" {
  role       = aws_iam_role.cluster.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
}

# Created up front so retention is ours, not "never expire".
resource "aws_cloudwatch_log_group" "this" {
  name              = "/aws/eks/${var.cluster_name}/cluster"
  retention_in_days = var.log_retention_days
}

resource "aws_eks_cluster" "this" {
  name     = var.cluster_name
  version  = var.kubernetes_version
  role_arn = aws_iam_role.cluster.arn

  enabled_cluster_log_types = var.enabled_log_types

  vpc_config {
    subnet_ids              = var.control_plane_subnet_ids
    endpoint_public_access  = true
    endpoint_private_access = true
    public_access_cidrs     = var.endpoint_public_access_cidrs
  }

  # Access entries only (no aws-auth ConfigMap). Whoever runs the first
  # apply becomes cluster admin; CI and extra admins come from var.cluster_admins.
  access_config {
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = true
  }

  # Avoid extended-support pricing if the version ages out.
  upgrade_policy {
    support_type = "STANDARD"
  }

  depends_on = [
    aws_iam_role_policy_attachment.cluster,
    aws_cloudwatch_log_group.this,
  ]
}

# --- Worker nodes ---------------------------------------------------------

data "aws_iam_policy_document" "node_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "node" {
  name               = "${var.cluster_name}-node-role"
  assume_role_policy = data.aws_iam_policy_document.node_assume.json
}

# Node role holds only what kubelet and the VPC CNI need. Application and
# agent permissions come from Pod Identity roles, never from here.
resource "aws_iam_role_policy_attachment" "node" {
  for_each = toset([
    "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
    "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
    "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
  ])

  role       = aws_iam_role.node.name
  policy_arn = each.value
}

# No image_id: EKS still picks the AMI for ami_type and supplies the bootstrap
# config; the NodeConfig below is merged into it by nodeadm (AL2023).
resource "aws_launch_template" "node" {
  name_prefix            = "${var.cluster_name}-node-"
  update_default_version = true

  # Must match the CNI's prefix delegation, or kubelet keeps the ENI-IP limit.
  user_data = base64encode(<<-EOT
    MIME-Version: 1.0
    Content-Type: multipart/mixed; boundary="BOUNDARY"

    --BOUNDARY
    Content-Type: application/node.eks.aws

    ---
    apiVersion: node.eks.aws/v1alpha1
    kind: NodeConfig
    spec:
      kubelet:
        config:
          maxPods: ${var.node_max_pods}

    --BOUNDARY--
  EOT
  )

  # IMDSv2 only, and a hop limit of 1 so pods (one network hop away) can't
  # reach instance metadata and borrow the node role. Workloads get AWS
  # access only through their own Pod Identity roles.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  block_device_mappings {
    device_name = "/dev/xvda"

    ebs {
      volume_size           = var.node_disk_size
      volume_type           = "gp3"
      encrypted             = true
      delete_on_termination = true
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags = {
      Name = "${var.cluster_name}-node"
    }
  }
}

resource "aws_eks_node_group" "default" {
  cluster_name = aws_eks_cluster.this.name
  # A prefix plus create_before_destroy: changes that force a new node group
  # bring the replacement up first, then drain and delete the old one.
  node_group_name_prefix = "default-"
  node_role_arn          = aws_iam_role.node.arn
  subnet_ids             = var.node_subnet_ids

  ami_type       = var.node_ami_type
  capacity_type  = var.node_capacity_type
  instance_types = var.node_instance_types

  launch_template {
    id      = aws_launch_template.node.id
    version = aws_launch_template.node.latest_version
  }

  # EKS tags the backing ASG with k8s.io/cluster-autoscaler/* automatically,
  # which cluster-autoscaler uses for auto-discovery.
  scaling_config {
    min_size     = var.node_min_size
    desired_size = var.node_desired_size
    max_size     = var.node_max_size
  }

  update_config {
    max_unavailable = 1
  }

  # cluster-autoscaler owns desired size after creation.
  lifecycle {
    create_before_destroy = true
    ignore_changes        = [scaling_config[0].desired_size]
  }

  depends_on = [
    aws_iam_role_policy_attachment.node,
    aws_eks_addon.before_nodes,
  ]
}

# --- Add-ons --------------------------------------------------------------

# Networking and Pod Identity must exist before nodes join; CoreDNS needs
# nodes to schedule onto, so it comes after.
resource "aws_eks_addon" "before_nodes" {
  for_each = toset(["vpc-cni", "kube-proxy", "eks-pod-identity-agent"])

  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = each.value
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "OVERWRITE"

  # Prefix delegation: the CNI assigns /28 prefixes to each ENI instead of
  # single IPs, lifting the per-node pod limit from the ENI-IP ceiling
  # (17 on t3.medium) to node_max_pods. Kubelet's maxPods is set to match in
  # the launch template.
  #
  # enableNetworkPolicy turns on the CNI's eBPF node agent, which enforces
  # Kubernetes NetworkPolicy objects. Namespaces without a policy stay open;
  # sensitive ones (mcp-server) deny ingress by default.
  configuration_values = each.value == "vpc-cni" ? jsonencode({
    enableNetworkPolicy = "true"
    env = {
      ENABLE_PREFIX_DELEGATION = "true"
      WARM_PREFIX_TARGET       = "1"
    }
  }) : null
}

resource "aws_eks_addon" "coredns" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "coredns"
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "OVERWRITE"

  depends_on = [aws_eks_node_group.default]
}

# --- Cluster access -------------------------------------------------------

resource "aws_eks_access_entry" "admin" {
  for_each = var.cluster_admins

  cluster_name  = aws_eks_cluster.this.name
  principal_arn = each.value
  type          = "STANDARD"
}

resource "aws_eks_access_policy_association" "admin" {
  for_each = aws_eks_access_entry.admin

  cluster_name  = aws_eks_cluster.this.name
  principal_arn = each.value.principal_arn
  policy_arn    = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"

  access_scope {
    type = "cluster"
  }
}
