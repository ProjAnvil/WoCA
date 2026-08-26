# Deployment inputs. Everything address-shaped is derived in compute.tf;
# this file only carries what the operator actually chooses.

variable "region" {
  type        = string
  default     = "us-east-1"
  description = "AWS region for the whole stack (single region, single AZ)."
}

variable "availability_zone" {
  type        = string
  default     = "us-east-1a"
  description = "Single-AZ placement for v1 (multi-AZ is a later profile)."
}

variable "arch" {
  type        = string
  default     = "arm64"
  description = "Instance architecture: arm64 (graviton) or x86_64. The ECR images must match."
}

variable "instance_type" {
  type        = string
  default     = "t4g.medium"
  description = "Instance type for every zone node (v1 keeps one size)."
}

variable "key_name" {
  type        = string
  description = "EC2 key pair for node SSH (admin_cidr gates access)."
}

variable "admin_cidr" {
  type        = string
  description = "CIDR allowed to SSH into nodes (e.g. your office range). No default: it must be a conscious choice."
}

variable "app_count" {
  type        = string # kept as string; validated 1..2
  default     = "1"
  description = "App-zone nodes. v1 maximum 2 — scaling beyond needs the shared sessions volume (EFS/NFS, gap-report P1)."
}

variable "woca_version" {
  type        = string
  default     = "local"
  description = "woca image tag from the release chain (P0-1); every woca image is pinned to it."
}

variable "registry" {
  type        = string
  default     = ""
  description = "ECCR registry base (account.dkr.ecr.region.amazonaws.com). Empty = third-party images pull from Docker Hub via NAT and woca images must be baked in (not supported in v1) — set it."
}

variable "dict_variant" {
  type        = string
  default     = "zh"
  description = "ladym CJK dictionary variant baked into the image (zh|zh_s|zh_t|jp) — informational for the ladym images push."
}

variable "allow_dmz_egress" {
  type        = bool
  default     = false
  description = "DMZ egress switch. false (default): the gateway node has no NAT route and its SG blocks egress — enterprise DMZ posture; gateway images must reach the node some other way (scp/docker load). true: adds the NAT route + opens egress for first-boot pulls — lock it back after."
}

variable "console_user" {
  type        = string
  default     = ""
  description = "Ops/config console login (WOCA_CONSOLE_USER). Empty = open console (dev posture — set both for production)."
}

variable "console_password" {
  type        = string
  default     = ""
  description = "Ops/config console password. Empty = generated (see outputs, sensitive)."
}

# ── data-plane passwords: generated when not provided ─────────────────────

variable "woca_pg_password" {
  type        = string
  default     = ""
  description = "woca OLTP PostgreSQL password. Empty = generated."
}

variable "woca_pg_repl_password" {
  type        = string
  default     = ""
  description = "PG streaming-replication password. Empty = generated."
}

variable "ladym_pg_password" {
  type        = string
  default     = ""
  description = "ladyM PostgreSQL password. Empty = generated."
}

variable "data_volume_gb" {
  type        = number
  default     = 50
  description = "EBS volume size for the data and memory nodes (PG data)."
}
