terraform {
  required_providers {
    keycloak = {
      source  = "keycloak/keycloak"
      version = "~> 5.6"
    }
  }
}

# Provider authentication is supplied by the caller's environment/backend.
# Never commit provider credentials or Terraform state.
provider "keycloak" {}

variable "realm_id" {
  type        = string
  description = "Existing realm in the installation's OIDC provider"
}
variable "client_id" {
  type    = string
  default = "release-control"
}
variable "public_url" {
  type        = string
  description = "External HTTPS origin of the RCP installation"
  validation {
    condition     = can(regex("^https://[^/?#]+$", var.public_url))
    error_message = "public_url must be an HTTPS origin without path/query/fragment."
  }
}
variable "access_role" {
  type        = string
  default     = "rcp-user"
  description = "Dedicated realm role granting human RCP access"
}

resource "keycloak_openid_client" "rcp" {
  realm_id                     = var.realm_id
  client_id                    = var.client_id
  name                         = "Release Control Plane"
  enabled                      = true
  access_type                  = "CONFIDENTIAL"
  standard_flow_enabled        = true
  direct_access_grants_enabled = false
  service_accounts_enabled     = false
  valid_redirect_uris          = ["${var.public_url}/auth/callback"]
  web_origins                  = [var.public_url]
  pkce_code_challenge_method   = "S256"
}
resource "keycloak_role" "rcp_access" {
  realm_id = var.realm_id
  name     = var.access_role
}
resource "keycloak_openid_user_realm_role_protocol_mapper" "roles" {
  realm_id            = var.realm_id
  client_id           = keycloak_openid_client.rcp.id
  name                = "RCP realm roles"
  claim_name          = "realm_access.roles"
  multivalued         = true
  add_to_id_token     = true
  add_to_access_token = true
  add_to_userinfo     = false
}
output "client_secret" {
  value     = keycloak_openid_client.rcp.client_secret
  sensitive = true
}
