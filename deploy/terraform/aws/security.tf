# Security groups: the stateful instance-level half of the policy.
# One SG per zone; a node carries the SG of every zone it has a leg in
# (its ENIs do), so "intra-zone all" is a self-referencing rule — the
# docker-bridge semantics translated 1:1.

resource "aws_security_group" "zone" {
  for_each = local.subnets

  name   = "${local.name}-${each.key}"
  vpc_id = aws_vpc.this.id

  # Intra-zone: everything from the same zone (docker bridge parity).
  ingress {
    description = "intra-${each.key} (docker-bridge semantics)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    self        = true
  }

  # SSH from the admin CIDR.
  ingress {
    description = "admin ssh"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.admin_cidr]
  }

  tags = { Name = "${local.name}-${each.key}", Zone = each.key }
}

# Egress is split out so the DMZ posture can switch without touching the
# other zones: every zone except dmz allows all egress (NAT-routed);
# dmz allows either VPC-only (locked, default) or all (first-boot pulls).
resource "aws_security_group_rule" "egress_open" {
  for_each = { for z in keys(local.subnets) : z => z if z != "dmz" }

  type              = "egress"
  security_group_id = aws_security_group.zone[each.value].id
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  cidr_blocks       = ["0.0.0.0/0"]
  description       = "zone egress (NAT-routed)"
}

resource "aws_security_group_rule" "dmz_egress" {
  type              = "egress"
  security_group_id = aws_security_group.zone["dmz"].id
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  cidr_blocks       = var.allow_dmz_egress ? ["0.0.0.0/0"] : [local.vpc_cidr]
  description       = var.allow_dmz_egress ? "dmz egress OPEN (first-boot image pulls — lock it back)" : "dmz egress: VPC only (locked posture)"
}

# The one genuinely cross-zone flow: gateway (dmz) → woca-server (app).
# Expresses the nginx upstreams of gateway/nginx-full.conf.
resource "aws_security_group_rule" "dmz_to_app" {
  for_each = toset(["8080", "8090"])

  type                     = "ingress"
  security_group_id        = aws_security_group.zone["app"].id
  source_security_group_id = aws_security_group.zone["dmz"].id
  from_port                = tonumber(each.value)
  to_port                  = tonumber(each.value)
  protocol                 = "tcp"
  description              = "gateway nginx → woca-server (${each.value})"
}
