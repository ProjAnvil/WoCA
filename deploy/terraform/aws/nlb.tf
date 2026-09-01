# The public entry: an internet NLB in the DMZ subnet targeting the
# gateway node's published host ports (the compose gateway ports block:
# 80 business, 8082 skill SPA, 8083 discovery, 8086 config console, plus
# 3000 = the Langfuse ops UI the gateway nginx reverse-proxies to the
# infra node).

locals {
  published = {
    business  = 80
    skills    = 8082
    discovery = 8083
    config    = 8086
    langfuse  = 3000
  }
}

resource "aws_lb" "dmz" {
  name               = "${local.name}-dmz"
  load_balancer_type = "network"

  # NLB with client-IP preservation: the DMZ NACL/SG rules for the
  # published ports admit real client sources.
  subnets = [aws_subnet.zone["dmz"].id]
  tags    = { Name = "${local.name}-dmz" }
}

resource "aws_lb_target_group" "gateway" {
  for_each = local.published

  name        = "${local.name}-${each.key}"
  target_type = "ip"
  port        = each.value
  protocol    = "TCP"
  vpc_id      = aws_vpc.this.id

  health_check {
    protocol = "TCP"
    port     = each.value
  }
}

resource "aws_lb_target_group_attachment" "gateway" {
  for_each = local.published

  target_group_arn = aws_lb_target_group.gateway[each.key].arn
  target_id        = local.node_ips["dmz"]
  port             = each.value
}

resource "aws_lb_listener" "published" {
  for_each = local.published

  load_balancer_arn = aws_lb.dmz.arn
  port              = each.value
  protocol          = "TCP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.gateway[each.key].arn
  }
}

# SG counterpart of the published ports (the NACL side lives in
# network.tf): the DMZ admits the service ports from anywhere — this is
# the whole point of a DMZ.
resource "aws_security_group_rule" "dmz_published" {
  for_each = local.published

  type              = "ingress"
  security_group_id = aws_security_group.zone["dmz"].id
  from_port         = each.value
  to_port           = each.value
  protocol          = "tcp"
  cidr_blocks       = ["0.0.0.0/0"]
  description       = "DMZ published port ${each.value} (${each.key})"
}
