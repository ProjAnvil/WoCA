# Zone-first network: one VPC, one subnet per zone, the DOCKER-USER
# isolation matrix as NACLs, and a NAT gateway that ONLY the non-DMZ
# subnets may route through.
#
# Five zones (dmz / app / data / infra / mgmt). Compose correspondence
# (docker-compose.full.yml networks):
#   woca-edge  → dmz (public, gateway half) + app (business half)
#   woca-pg    → data
#   woca-mq + ladym-data → infra (platform runtime dependencies)
#   woca-mgmt  → mgmt minus nacos (nacos → infra: nacosrt is a hard
#                startup gate for every business role, which makes it
#                runtime infrastructure, not a management surface — the
#                mgmt zone must be free to fail without blocking role
#                restarts/scale-outs)

locals {
  name = "woca-private"

  # The compose 172.28.x segments, re-based into a dedicated VPC CIDR.
  vpc_cidr = "10.50.0.0/16"
  subnets = {
    dmz   = { cidr = "10.50.10.0/24", public = true }  # gateway half of woca-edge
    app   = { cidr = "10.50.20.0/24", public = false } # business half of woca-edge
    data  = { cidr = "10.50.30.0/24", public = false } # woca-pg
    infra = { cidr = "10.50.40.0/24", public = false } # woca-mq + ladym-data (+ nacos)
    mgmt  = { cidr = "10.50.60.0/24", public = false } # consoles (ops, config)
  }

  # Legal CROSS-subnet flows — everything else inside the VPC is denied at
  # the NACL (the DOCKER-USER matrix made real). Dual-homed services carry
  # their other legs as ENIs, so nearly all compose traffic stays
  # intra-subnet; the genuinely cross-subnet flows are the gateway (dmz) →
  # woca-server (app) business proxy target and the gateway → langfuse-web
  # (infra) ops-UI reverse proxy.
  cross_flows = [
    { from = "dmz", to = "app", port = 8080, proto = "tcp", note = "gateway nginx → woca-server business entry" },
    { from = "dmz", to = "app", port = 8090, proto = "tcp", note = "gateway nginx → woca-server service-plane listener (WOCA_GW_ADDR)" },
    { from = "dmz", to = "infra", port = 3000, proto = "tcp", note = "gateway nginx → langfuse-web ops UI (NLB :3000)" },
  ]
}

resource "aws_vpc" "this" {
  cidr_block           = local.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = local.name }
}

resource "aws_subnet" "zone" {
  for_each = local.subnets

  vpc_id                  = aws_vpc.this.id
  availability_zone       = var.availability_zone
  cidr_block              = each.value.cidr
  map_public_ip_on_launch = each.value.public
  tags = {
    Name = "${local.name}-${each.key}"
    Zone = each.key
  }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = { Name = local.name }
}

# The NAT gateway lives in the DMZ subnet (it needs the IGW route) but the
# DMZ *instances* have no route to it unless allow_dmz_egress is set —
# egress control is a route-table property, not a placement one.
resource "aws_eip" "nat" {
  domain = "vpc"
  tags   = { Name = "${local.name}-nat" }
}

resource "aws_nat_gateway" "this" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.zone["dmz"].id
  tags          = { Name = local.name }
  depends_on    = [aws_internet_gateway.this]
}

# Route tables: public (dmz + NAT) vs private (NAT default route). The DMZ
# instance route table gets the NAT route ONLY when allow_dmz_egress.
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }
  tags = { Name = "${local.name}-public" }
}

resource "aws_route_table" "private" {
  for_each = local.subnets

  vpc_id = aws_vpc.this.id
  tags   = { Name = "${local.name}-${each.key}" }
}

resource "aws_route" "private_nat" {
  for_each = { for z, s in local.subnets : z => s if !s.public || var.allow_dmz_egress }

  route_table_id         = aws_route_table.private[each.key].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.this.id
}

resource "aws_route_table_association" "zone" {
  for_each = local.subnets

  subnet_id      = aws_subnet.zone[each.key].id
  route_table_id = aws_route_table.private[each.key].id
}

# ── NACLs: the isolation matrix, stateless ────────────────────────────────
#
# Rule order per subnet: intra-subnet allow → ephemeral returns from
# cross-flow sources → explicit cross flows → SSH from admin → deny rest
# of the VPC → (dmz only) allow the published service ports from the
# internet (the NLB preserves client IPs).

resource "aws_network_acl" "zone" {
  for_each = local.subnets

  vpc_id     = aws_vpc.this.id
  subnet_ids = [aws_subnet.zone[each.key].id]
  tags       = { Name = "${local.name}-${each.key}" }
}

# Intra-subnet: all protocols, both directions (docker-bridge semantics).
resource "aws_network_acl_rule" "intra_in" {
  for_each = local.subnets

  network_acl_id = aws_network_acl.zone[each.key].id
  rule_number    = 100
  egress         = false
  protocol       = "-1"
  rule_action    = "allow"
  cidr_block     = each.value.cidr
}

resource "aws_network_acl_rule" "intra_out" {
  for_each = local.subnets

  network_acl_id = aws_network_acl.zone[each.key].id
  rule_number    = 100
  egress         = true
  protocol       = "-1"
  rule_action    = "allow"
  cidr_block     = each.value.cidr
}

# Explicit cross flows + their stateless return paths.
resource "aws_network_acl_rule" "cross_in" {
  for_each = { for i, f in local.cross_flows : "${f.from}->${f.to}:${f.port}" => f }

  network_acl_id = aws_network_acl.zone[each.value.to].id
  rule_number    = 110 + index(local.cross_flows, each.value)
  egress         = false
  protocol       = "tcp"
  from_port      = each.value.port
  to_port        = each.value.port
  rule_action    = "allow"
  cidr_block     = local.subnets[each.value.from].cidr
}

# Return traffic for connections initiated by this subnet's nodes toward
# another subnet (e.g. app → NAT in dmz) and cross-flow replies: ephemeral
# ports from the VPC. Honest note: this widens the inter-zone deny — a
# cross-zone packet aimed at an ephemeral port passes the NACL — the SGs
# (stateful) carry the precise policy; the NACL is the coarse layer.
resource "aws_network_acl_rule" "ephemeral_in" {
  for_each = local.subnets

  network_acl_id = aws_network_acl.zone[each.key].id
  rule_number    = 140
  egress         = false
  protocol       = "tcp"
  from_port      = 1024
  to_port        = 65535
  rule_action    = "allow"
  cidr_block     = local.vpc_cidr
}

# Outbound: allow everything (return-path restrictions would need the
# mirrored rule set; the SGs carry the egress posture).
resource "aws_network_acl_rule" "out_all" {
  for_each = local.subnets

  network_acl_id = aws_network_acl.zone[each.key].id
  rule_number    = 150
  egress         = true
  protocol       = "-1"
  rule_action    = "allow"
  cidr_block     = "0.0.0.0/0"
}

# SSH from the admin CIDR only.
resource "aws_network_acl_rule" "ssh_in" {
  for_each = local.subnets

  network_acl_id = aws_network_acl.zone[each.key].id
  rule_number    = 160
  egress         = false
  protocol       = "tcp"
  from_port      = 22
  to_port        = 22
  rule_action    = "allow"
  cidr_block     = var.admin_cidr
}

# Deny everything else inside the VPC inbound (the DOCKER-USER matrix).
# Egress stays allow-all (rule 150) — the stateful SGs govern egress.
resource "aws_network_acl_rule" "deny_vpc_in" {
  for_each = local.subnets

  network_acl_id = aws_network_acl.zone[each.key].id
  rule_number    = 900
  egress         = false
  protocol       = "-1"
  rule_action    = "deny"
  cidr_block     = local.vpc_cidr
}

# DMZ only: the published service ports from the internet (client-IP
# preserving NLB). 80/8082/8083/8086 = the compose gateway ports block;
# 3000 = the Langfuse ops UI (reverse-proxied by the gateway nginx).
resource "aws_network_acl_rule" "dmz_public_in" {
  count = length([80, 3000, 8082, 8083, 8086])

  network_acl_id = aws_network_acl.zone["dmz"].id
  rule_number    = 200 + count.index
  egress         = false
  protocol       = "tcp"
  from_port      = element([80, 3000, 8082, 8083, 8086], count.index)
  to_port        = element([80, 3000, 8082, 8083, 8086], count.index)
  rule_action    = "allow"
  cidr_block     = "0.0.0.0/0"
}

# DMZ internet return traffic for its own (locked-down) egress.
resource "aws_network_acl_rule" "dmz_ephemeral_in" {
  network_acl_id = aws_network_acl.zone["dmz"].id
  rule_number    = 240
  egress         = false
  protocol       = "tcp"
  from_port      = 1024
  to_port        = 65535
  rule_action    = "allow"
  cidr_block     = "0.0.0.0/0"
}
