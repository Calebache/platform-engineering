# IAM permission policies passed into the github-oidc and pod-identity-role
# modules. Each grants one workload only what it needs.

data "aws_route53_zone" "main" {
  name         = var.domain_name
  private_zone = false
}

# --- GitHub Actions -------------------------------------------------------

# Plan takes the state lock, which means writing/deleting the .tflock object.
data "aws_iam_policy_document" "plan_state_lock" {
  statement {
    sid       = "StateLock"
    actions   = ["s3:PutObject", "s3:DeleteObject"]
    resources = ["arn:aws:s3:::${var.state_bucket_name}/*.tflock"]
  }
}

data "aws_iam_policy_document" "image_builder" {
  statement {
    sid       = "EcrLogin"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  statement {
    sid = "PushToPlatformRepos"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:BatchGetImage",
      "ecr:CompleteLayerUpload",
      "ecr:DescribeImageScanFindings",
      "ecr:DescribeImages",
      "ecr:InitiateLayerUpload",
      "ecr:PutImage",
      "ecr:UploadLayerPart",
    ]
    resources = values(module.ecr.repository_arns)
  }
}

# --- sre-agent ------------------------------------------------------------
# Can call Claude on Bedrock and append audit records. Nothing else: any
# other AWS call (doc.md §17) returns AccessDenied. It cannot read, delete
# or list audit records.

data "aws_iam_policy_document" "sre_agent" {
  statement {
    sid     = "InvokeClaudeOnBedrock"
    actions = ["bedrock-mantle:CreateInference"]
    # TODO: scope to the Opus 5.5 model ARN once the bedrock-mantle resource
    # ARN format is confirmed in the IAM service authorization reference.
    resources = ["*"]

    # The Mantle endpoint doesn't exist in the cluster region, so the agent
    # calls bedrock_region; allow nothing else.
    condition {
      test     = "StringEquals"
      variable = "aws:RequestedRegion"
      values   = [var.bedrock_region]
    }
  }

  statement {
    sid       = "WriteAuditRecords"
    actions   = ["s3:PutObject"]
    resources = ["${module.audit_bucket.bucket_arn}/audit/*"]
  }
}

# --- external-dns ---------------------------------------------------------

data "aws_iam_policy_document" "external_dns" {
  statement {
    sid = "ManageRecordsInPlatformZone"
    actions = [
      "route53:ChangeResourceRecordSets",
      "route53:ListResourceRecordSets",
      "route53:ListTagsForResource",
      "route53:ListTagsForResources",
    ]
    resources = [data.aws_route53_zone.main.arn]
  }

  statement {
    sid       = "DiscoverZones"
    actions   = ["route53:ListHostedZones"]
    resources = ["*"]
  }
}

# --- cert-manager (DNS-01 via Route53) ------------------------------------

data "aws_iam_policy_document" "cert_manager" {
  statement {
    sid       = "GetChange"
    actions   = ["route53:GetChange"]
    resources = ["arn:aws:route53:::change/*"]
  }

  statement {
    sid = "SolveChallengesInPlatformZone"
    actions = [
      "route53:ChangeResourceRecordSets",
      "route53:ListResourceRecordSets",
    ]
    resources = [data.aws_route53_zone.main.arn]
  }

  statement {
    sid       = "FindZone"
    actions   = ["route53:ListHostedZonesByName"]
    resources = ["*"]
  }
}

# --- cluster-autoscaler ---------------------------------------------------
# Mutating calls are limited to ASGs tagged as owned by this cluster.

data "aws_iam_policy_document" "cluster_autoscaler" {
  statement {
    sid = "Describe"
    actions = [
      "autoscaling:DescribeAutoScalingGroups",
      "autoscaling:DescribeAutoScalingInstances",
      "autoscaling:DescribeLaunchConfigurations",
      "autoscaling:DescribeScalingActivities",
      "autoscaling:DescribeTags",
      "ec2:DescribeImages",
      "ec2:DescribeInstanceTypes",
      "ec2:DescribeLaunchTemplateVersions",
      "ec2:GetInstanceTypesFromInstanceRequirements",
      "eks:DescribeNodegroup",
    ]
    resources = ["*"]
  }

  statement {
    sid = "ScaleOwnedNodeGroups"
    actions = [
      "autoscaling:SetDesiredCapacity",
      "autoscaling:TerminateInstanceInAutoScalingGroup",
    ]
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/k8s.io/cluster-autoscaler/${local.cluster_name}"
      values   = ["owned"]
    }
  }
}
