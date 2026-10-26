# Node topology: one EC2 node per zone (app scales to 2), legs as ENIs.
# The leg matrix is the compose service→network attachment, translated
# into the five-zone model (mq+memory+nacos = infra):
#
#   compose service      networks                     → node   ENIs
#   gateway              edge, ladym-data, mgmt       → dmz    dmz, infra, mgmt
#   woca-scheduler/worker   edge, pg, mq, ladym, mgmt    → app    app, data, infra
#         (the compose mgmt leg exists only for nacos; with nacos in
#          infra it folds into the infra leg — 5 legs → 3)
#   woca-tenant          edge, pg, mq                 → app    (covered by app legs)
#   woca-pg-*            pg                           → data   data
#   nats-js-*            mq  \                         → infra  infra
#   ladym-*              ladym-data  > merged zone     → infra  (same subnet)
#   nacos + nacos-init   mgmt → infra (hard runtime    → infra  (same subnet)
#                         gate = infrastructure)
#   woca-config          mgmt, pg, mq                 → mgmt   mgmt, data, infra
#   woca-ops             mgmt, edge                   → mgmt   mgmt, app

data "aws_ssm_parameter" "al2023" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-${var.arch}"
}

locals {
  ami = data.aws_ssm_parameter.al2023.value

  # Stable primary IPs so every cross-node reference (nacos DSN, nginx
  # upstreams, nacos-init publishes) is deterministic.
  node_ips = {
    dmz   = "10.50.10.10"
    app-1 = "10.50.20.11"
    app-2 = "10.50.20.12"
    data  = "10.50.30.10"
    infra = "10.50.40.10"
    mgmt  = "10.50.60.10"
  }

  app_nodes = var.app_count == "2" ? ["app-1", "app-2"] : ["app-1"]

  # zone → (subnet_id, sg_id), used by every ENI.
  zone_refs = {
    for z, s in local.subnets : z => {
      subnet = aws_subnet.zone[z].id
      sg     = aws_security_group.zone[z].id
    }
  }

  # The leg matrix for the ACTIVE node set: node → ordered legs. Index 0
  # is the primary interface (fixed private IP), the rest are attachments.
  app_legs = ["app", "data", "infra"]
  base_legs = {
    dmz   = ["dmz", "infra", "mgmt"]
    data  = ["data"]
    infra = ["infra"]
    mgmt  = ["mgmt", "data", "infra", "app"]
  }
  active_nodes = concat(["dmz", "data", "infra", "mgmt"], local.app_nodes)
  legs         = merge(local.base_legs, { for n in local.app_nodes : n => local.app_legs })
}

# Extra ENIs (device_index 1..n): one per non-primary leg.
resource "aws_network_interface" "leg" {
  for_each = { for pair in flatten([for node, zs in local.legs : [for z in slice(zs, 1, length(zs)) : "${node}:${z}"]]) : pair => pair }

  subnet_id       = local.zone_refs[split(":", each.value)[1]].subnet
  security_groups = [local.zone_refs[split(":", each.value)[1]].sg]
  tags = {
    Name = "${local.name}-${replace(each.value, ":", "-")}"
    Node = split(":", each.value)[0]
  }
}

locals {
  # instance → map device_index → {interface_id, network_ip, device_index}
  # (primary ENIs are inline on the instance; extras come from the map.)
  leg_attachments = {
    for node, zs in local.legs : node => {
      for i, z in slice(zs, 1, length(zs)) :
      tostring(i + 1) => { id = aws_network_interface.leg["${node}:${z}"].id }
    }
  }

  node_tags = {
    dmz   = "woca-private-dmz"
    app-1 = "woca-private-app-1"
    app-2 = "woca-private-app-2"
    data  = "woca-private-data"
    infra = "woca-private-infra"
    mgmt  = "woca-private-mgmt"
  }
}

resource "aws_instance" "node" {
  for_each = toset(local.active_nodes)

  ami           = local.ami
  instance_type = var.instance_type
  key_name      = var.key_name

  # Primary interface: the node's home zone, fixed IP.
  network_interface {
    device_index         = 0
    network_card_index   = 0
    network_interface_id = aws_network_interface.primary[each.key].id
  }

  # Bridge legs: one ENI per additional zone, device index 1..n.
  dynamic "network_interface" {
    for_each = local.leg_attachments[each.key]
    content {
      device_index         = tonumber(network_interface.key)
      network_card_index   = 0
      network_interface_id = network_interface.value.id
    }
  }

  root_block_device {
    volume_size = 20
    volume_type = "gp3"
    encrypted   = true
  }

  user_data                   = base64gzip(local.user_data[each.key])
  user_data_replace_on_change = true

  tags = { Name = local.node_tags[each.key] }
}

# Primary ENIs as standalone resources so the fixed IP is explicit.
resource "aws_network_interface" "primary" {
  for_each = toset(local.active_nodes)

  subnet_id       = local.zone_refs[local.legs[each.key][0]].subnet
  private_ips     = [local.node_ips[each.key]]
  security_groups = [local.zone_refs[local.legs[each.key][0]].sg]
  tags            = { Name = "${local.name}-${each.key}-primary" }
}

# Data-plane volumes: PG data (data + infra nodes — ladym-pg lives on
# infra) and the session transcripts (app nodes). Encrypted gp3.
resource "aws_ebs_volume" "data" {
  for_each = toset(["data", "infra"])

  availability_zone = var.availability_zone
  size              = var.data_volume_gb
  type              = "gp3"
  encrypted         = true
  tags              = { Name = "${local.name}-${each.value}-data" }
}

resource "aws_volume_attachment" "data" {
  for_each = toset(["data", "infra"])

  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.data[each.value].id
  instance_id = aws_instance.node[each.value].id
}

resource "aws_ebs_volume" "sessions" {
  for_each = toset(local.app_nodes)

  availability_zone = var.availability_zone
  size              = var.data_volume_gb
  type              = "gp3"
  encrypted         = true
  tags              = { Name = "${local.name}-${each.key}-sessions" }
}

resource "aws_volume_attachment" "sessions" {
  for_each = toset(local.app_nodes)

  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.sessions[each.key].id
  instance_id = aws_instance.node[each.key].id
}
