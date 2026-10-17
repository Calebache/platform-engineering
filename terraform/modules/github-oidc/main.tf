# GitHub Actions -> OIDC -> STS AssumeRoleWithWebIdentity. No static AWS keys
# live in GitHub. Each role trusts exactly one `sub` claim, so a workflow can
# only get the role that matches where it runs.

# An account can hold only one provider per URL. The first environment in an
# account creates it; later ones set create_oidc_provider = false and look it up.
resource "aws_iam_openid_connect_provider" "github" {
  count = var.create_oidc_provider ? 1 : 0

  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]
}

data "aws_iam_openid_connect_provider" "github" {
  count = var.create_oidc_provider ? 0 : 1

  url = "https://token.actions.githubusercontent.com"
}

locals {
  oidc_provider_arn = var.create_oidc_provider ? aws_iam_openid_connect_provider.github[0].arn : data.aws_iam_openid_connect_provider.github[0].arn

  # Keys come from map keys and static ARNs, so for_each stays plannable even
  # when inline policy JSON is unknown.
  managed_policy_attachments = merge([
    for role, cfg in var.roles : {
      for arn in cfg.managed_policy_arns : "${role}|${arn}" => { role = role, policy_arn = arn }
    }
  ]...)

  inline_policies = merge([
    for role, cfg in var.roles : {
      for name, json in cfg.inline_policies : "${role}|${name}" => { role = role, name = name, policy = json }
    }
  ]...)
}

data "aws_iam_policy_document" "assume" {
  for_each = var.roles

  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [local.oidc_provider_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:sub"
      values   = [each.value.subject]
    }
  }
}

resource "aws_iam_role" "this" {
  for_each = var.roles

  name                 = "${var.name_prefix}-${each.key}"
  assume_role_policy   = data.aws_iam_policy_document.assume[each.key].json
  max_session_duration = var.max_session_duration
}

resource "aws_iam_role_policy_attachment" "this" {
  for_each = local.managed_policy_attachments

  role       = aws_iam_role.this[each.value.role].name
  policy_arn = each.value.policy_arn
}

resource "aws_iam_role_policy" "this" {
  for_each = local.inline_policies

  name   = each.value.name
  role   = aws_iam_role.this[each.value.role].id
  policy = each.value.policy
}
