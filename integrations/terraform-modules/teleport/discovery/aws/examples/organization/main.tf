module "aws_discovery" {
  source = "../.."

  teleport_proxy_public_addr    = "example.teleport.sh:443"
  teleport_discovery_group_name = "cloud-discovery-group"

  # Enroll resources from all AWS Accounts in the Organization
  # Only EC2 resource discovery is supported for organization-wide discovery.
  enroll_organization_accounts = true
  filter_organizational_units = {
    # Include accounts under any Organizational Unit.
    include = ["*"]
    # Exclude a specific Organizational Unit and all the accounts under it.
    # Takes precedence over the include rule, so accounts under this OU will not be enrolled even if the include rule matches them.
    exclude = ["ou-5678-production"]
  }

  # Discover EC2 instances with matching rules
  aws_matchers = [
    {
      types = ["ec2"]
      # EC2 discovery supports a wildcard to find instances in all regions.
      regions = ["*"]
      tags = {
        env = ["prod"]
      }
    }
  ]

  # Apply the additional Teleport label "origin=example" to all Teleport resources created by this module
  apply_teleport_resource_labels = { origin = "example" }
  # Apply the additional AWS tag "origin=example" to all AWS resources created by this module
  apply_aws_tags = { origin = "example" }
}