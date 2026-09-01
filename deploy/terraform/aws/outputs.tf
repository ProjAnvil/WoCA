output "nlb_dns" {
  value       = aws_lb.dmz.dns_name
  description = "The public entry (business :80, skills :9082, discovery :9083, config console :9086, Langfuse UI :3000)."
}

output "node_ips" {
  value       = { for node, ip in local.node_ips : node => ip if contains(concat(["dmz", "data", "mq", "memory", "mgmt"], local.app_nodes), node) }
  description = "Stable primary IP of every zone node (ENI legs add the cross-zone interfaces)."
}

output "console_password" {
  value       = local.console_password
  sensitive   = true
  description = "Generated ops/config console password (when var.console_password was empty)."
}

output "woca_pg_password" {
  value     = local.woca_pg_password
  sensitive = true
}

output "woca_pg_repl_password" {
  value     = local.woca_pg_repl_password
  sensitive = true
}

output "ladym_pg_password" {
  value     = local.ladym_pg_password
  sensitive = true
}

output "ecr_repositories" {
  value       = { for repo, r in aws_ecr_repository.this : repo => r.repository_url }
  description = "Push the woca + ladym images here before `terraform apply` (tag = var.woca_version)."
}

output "langfuse_admin_password" {
  value       = local.langfuse_admin_password
  sensitive   = true
  description = "Generated Langfuse bootstrap admin password (admin@woca.local) — UI on NLB :3000 (when var.langfuse_admin_password was empty)."
}
