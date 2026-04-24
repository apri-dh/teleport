################################################################################
# AWS IAM policy and attachment for Teleport Discovery Service
################################################################################

locals {
  aws_iam_policy_name_prefix = (
    var.aws_iam_policy_use_name_prefix
    ? "${var.aws_iam_policy_name}-"
    : null
  )
  aws_iam_policy_name = (
    var.aws_iam_policy_use_name_prefix
    ? null
    : var.aws_iam_policy_name
  )

  uses_ec2 = contains(local.aws_matcher_types, "ec2")
  uses_eks = contains(local.aws_matcher_types, "eks")

  ec2_actions = concat(
    contains(local.aws_matcher_regions, "*") ? ["account:ListRegions"] : [],
    [
      "ec2:DescribeInstances",
      "ssm:DescribeInstanceInformation",
      "ssm:GetCommandInvocation",
      "ssm:ListCommandInvocations",
      "ssm:SendCommand",
    ]
  )

  # TODO(charles): add account:ListRegions when EKS supports wildcard region
  eks_actions = [
    "eks:ListClusters",
    "eks:DescribeCluster",
    "eks:ListAccessEntries",
    "eks:DescribeAccessEntry",
    "eks:CreateAccessEntry",
    "eks:DeleteAccessEntry",
    "eks:AssociateAccessPolicy",
    "eks:TagResource",
    "eks:UpdateAccessEntry",
  ]

  policy_actions = concat(
    local.uses_ec2 ? local.ec2_actions : [],
    local.uses_eks ? local.eks_actions : [],
  )
}

data "aws_iam_policy_document" "teleport_discovery_resource_enrollment" {
  count = local.create ? 1 : 0

  statement {
    effect    = "Allow"
    actions   = local.policy_actions
    resources = ["*"]
  }
}

data "aws_iam_policy_document" "teleport_organization_discovery" {
  count = local.create && local.organization_deployment ? 1 : 0

  # Allow listing accounts in the organization.
  statement {
    effect = "Allow"

    actions = [
      # Required for enumerating all the accounts under the organization.
      "organizations:ListAccountsForParent",
      "organizations:ListChildren",
      "organizations:ListRoots",
      # Allow Teleport to verify that an account belongs to an organization.
      "organizations:DescribeAccount"
    ]

    resources = ["*"]
  }

  # Allow assuming the role created in member accounts.
  statement {
    effect    = "Allow"
    actions   = ["sts:AssumeRole"]
    resources = ["arn:${local.aws_partition}:iam::*:role/${var.aws_iam_role_name_for_child_accounts}"]
  }
}

resource "aws_iam_policy" "teleport_discovery_service" {
  count = local.create ? 1 : 0

  description = "AWS IAM policy that grants the permissions needed for Teleport to discover resources in AWS."
  name        = local.aws_iam_policy_name
  name_prefix = local.aws_iam_policy_name_prefix
  path        = "/"
  tags        = local.apply_aws_tags
  policy = coalesce(
    var.aws_iam_policy_document,
    (
      local.single_account_deployment ?
      data.aws_iam_policy_document.teleport_discovery_resource_enrollment[0].json :
      data.aws_iam_policy_document.teleport_organization_discovery[0].json
    )
  )
}

data "aws_iam_policy_document" "allow_assume_role_for_child_accounts" {
  count = local.create && local.organization_deployment ? 1 : 0

  statement {
    principals {
      type        = "AWS"
      identifiers = [aws_iam_role.teleport_discovery_service[0].arn]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:PrincipalOrgID"
      values   = [local.aws_organization_id]
    }

    effect = "Allow"

    actions = ["sts:AssumeRole"]
  }
}

################################################################################
# AWS IAM policy attachment for Teleport Discovery Service
################################################################################

resource "aws_iam_role_policy_attachment" "teleport_discovery_service" {
  count = local.create ? 1 : 0

  policy_arn = one(aws_iam_policy.teleport_discovery_service[*].arn)
  # we already know the role name, but use expression reference to establish
  # dependency on the role's existence
  role = one(aws_iam_role.teleport_discovery_service[*].name)
}
