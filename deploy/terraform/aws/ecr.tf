# ECR repositories for the deployment's own images. Third-party images
# (postgres/nats/nacos/nginx/pgvector/…) pull from Docker Hub through the
# NAT by default; air-gap deployments retag+push them here too.

locals {
  image_repos = [
    "woca-scheduler",
    "woca-worker",
    "woca-ops",
    "woca-config",
    "woca-tenant",
    "ladym-enterprise",
  ]
}

resource "aws_ecr_repository" "this" {
  for_each = toset(local.image_repos)

  name                 = "${local.name}-${replace(each.value, "/", "-")}"
  image_tag_mutability = "IMMUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }

  tags = { Name = "${local.name}-${each.value}" }
}

locals {
  # Full image references used by the compose subsets.
  images = {
    for repo in local.image_repos :
    repo => "${aws_ecr_repository.this[repo].repository_url}:${var.woca_version}"
  }
}
