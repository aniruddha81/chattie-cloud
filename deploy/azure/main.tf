# Chattie on Azure: a load balancer, app VMs, a managed Postgres database
# (Azure Database for PostgreSQL) and one small VM for Redis.
#
#   internet -> load balancer :80 -> app VMs :8080 -> managed Postgres
#                                                  -> Redis VM
#
# A learning setup: plain HTTP, and destroy deletes the database.
# This is a separate environment from AWS, with its own users and messages.

terraform {
  required_providers {
    azurerm = { source = "hashicorp/azurerm", version = ">= 5.0" }
    random  = { source = "hashicorp/random", version = ">= 3.0" }
    tls     = { source = "hashicorp/tls", version = ">= 4.0" }
  }
}

provider "azurerm" {
  subscription_id = var.subscription_id
  features {
    # Let destroy remove the whole resource group even if something inside it
    # was not created by Terraform, so nothing is left behind to bill.
    resource_group {
      prevent_deletion_if_contains_resources = false
    }
  }
}

variable "subscription_id" {
  description = "The Subscription ID shown under Subscriptions in the Azure portal"
}

variable "location" {
  description = "A region your subscription may use. Student subscriptions allow only a few."
  default     = "centralindia"
}

variable "app_count" {
  description = "How many chat VMs to run."
  default     = 2
}

# 2 vCPUs and 1 GB each. Student subscriptions allow 6 vCPUs per region, so
# three VMs of this size use all of it.
variable "app_size" {
  default = "Standard_B2ats_v2"
}

variable "redis_size" {
  default = "Standard_B2ats_v2"
}

variable "db_size" {
  description = "The managed database size. B_Standard_B1ms is the smallest."
  default     = "B_Standard_B1ms"
}

variable "image" {
  description = "The image GitHub Actions published. Use a commit tag instead of latest to deploy a specific version."
  default     = "ghcr.io/aniruddha81/chattie-cloud:latest"
}

locals {
  redis_ip = "10.1.0.10"
}

resource "random_string" "suffix" {
  length  = 6
  special = false
  upper   = false
}

resource "azurerm_resource_group" "main" {
  name     = "chattie"
  location = var.location
}

# ---------- network ----------

resource "azurerm_virtual_network" "main" {
  name                = "chattie"
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  address_space       = ["10.1.0.0/16"]
}

resource "azurerm_subnet" "main" {
  name                 = "vms"
  resource_group_name  = azurerm_resource_group.main.name
  virtual_network_name = azurerm_virtual_network.main.name
  address_prefixes     = ["10.1.0.0/24"]
}

# The database gets its own subnet, handed over ("delegated") to the database
# service. Nothing else can be placed in it.
resource "azurerm_subnet" "db" {
  name                 = "db"
  resource_group_name  = azurerm_resource_group.main.name
  virtual_network_name = azurerm_virtual_network.main.name
  address_prefixes     = ["10.1.1.0/24"]
  delegation {
    name = "postgres"
    service_delegation {
      name    = "Microsoft.DBforPostgreSQL/flexibleServers"
      actions = ["Microsoft.Network/virtualNetworks/subnets/join/action"]
    }
  }
}

# The only port open to the internet is the chat port. Redis is reachable
# only from inside the virtual network (Azure's default rule), and no VM has
# a public address of its own.
resource "azurerm_network_security_group" "main" {
  name                = "chattie"
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  security_rule {
    name                       = "chat"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "8080"
    source_address_prefix      = "Internet"
    destination_address_prefix = "*"
  }
}

resource "azurerm_subnet_network_security_group_association" "main" {
  subnet_id                 = azurerm_subnet.main.id
  network_security_group_id = azurerm_network_security_group.main.id
}

# ---------- load balancer ----------

resource "azurerm_public_ip" "lb" {
  name                = "chattie"
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  allocation_method   = "Static"
  sku                 = "Standard"
  domain_name_label   = "chattie-${random_string.suffix.result}"
}

resource "azurerm_lb" "main" {
  name                = "chattie"
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  sku                 = "Standard"
  frontend_ip_configuration {
    name                 = "public"
    public_ip_address_id = azurerm_public_ip.lb.id
  }
}

# Two pools: "app" receives chat traffic, "outbound" gives every VM a way out
# to the internet (to install Docker and pull images).
resource "azurerm_lb_backend_address_pool" "app" {
  name            = "app"
  loadbalancer_id = azurerm_lb.main.id
}

resource "azurerm_lb_backend_address_pool" "outbound" {
  name            = "outbound"
  loadbalancer_id = azurerm_lb.main.id
}

# The load balancer only sends traffic to VMs whose /readyz answers 200.
resource "azurerm_lb_probe" "ready" {
  name                = "ready"
  loadbalancer_id     = azurerm_lb.main.id
  protocol            = "Http"
  port                = 8080
  request_path        = "/readyz"
  interval_in_seconds = 5
}

resource "azurerm_lb_rule" "http" {
  name                           = "http"
  loadbalancer_id                = azurerm_lb.main.id
  protocol                       = "Tcp"
  frontend_port                  = 80
  backend_port                   = 8080
  frontend_ip_configuration_name = "public"
  backend_address_pool_ids       = [azurerm_lb_backend_address_pool.app.id]
  probe_id                       = azurerm_lb_probe.ready.id
  disable_outbound_snat          = true
}

resource "azurerm_lb_outbound_rule" "internet" {
  name                    = "internet"
  loadbalancer_id         = azurerm_lb.main.id
  protocol                = "All"
  backend_address_pool_id = azurerm_lb_backend_address_pool.outbound.id
  frontend_ip_configuration {
    name = "public"
  }
}

# ---------- network interfaces ----------

resource "azurerm_network_interface" "redis" {
  name                = "chattie-redis"
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  ip_configuration {
    name                          = "internal"
    subnet_id                     = azurerm_subnet.main.id
    private_ip_address_allocation = "Static"
    private_ip_address            = local.redis_ip
  }
}

resource "azurerm_network_interface" "app" {
  count               = var.app_count
  name                = "chattie-app-${count.index + 1}"
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  ip_configuration {
    name                          = "internal"
    subnet_id                     = azurerm_subnet.main.id
    private_ip_address_allocation = "Dynamic"
  }
}

resource "azurerm_network_interface_backend_address_pool_association" "app" {
  count                   = var.app_count
  network_interface_id    = azurerm_network_interface.app[count.index].id
  ip_configuration_name   = "internal"
  backend_address_pool_id = azurerm_lb_backend_address_pool.app.id
}

resource "azurerm_network_interface_backend_address_pool_association" "outbound" {
  count                   = var.app_count + 1
  network_interface_id    = concat(azurerm_network_interface.app[*].id, [azurerm_network_interface.redis.id])[count.index]
  ip_configuration_name   = "internal"
  backend_address_pool_id = azurerm_lb_backend_address_pool.outbound.id
}

# ---------- database ----------

resource "random_password" "db" {
  length  = 32
  special = false
}

# The database has no public address. This private DNS zone lets the VMs find
# it by name inside the virtual network.
resource "azurerm_private_dns_zone" "db" {
  name                = "chattie.postgres.database.azure.com"
  resource_group_name = azurerm_resource_group.main.name
}

resource "azurerm_private_dns_zone_virtual_network_link" "db" {
  name                = "chattie"
  private_dns_zone_id = azurerm_private_dns_zone.db.id
  virtual_network_id  = azurerm_virtual_network.main.id
}

# Managed Postgres: Azure stores the data outside any VM, patches the server
# and keeps seven days of backups.
resource "azurerm_postgresql_flexible_server" "main" {
  name                          = "chattie-${random_string.suffix.result}"
  resource_group_name           = azurerm_resource_group.main.name
  location                      = azurerm_resource_group.main.location
  version                       = "18"
  sku_name                      = var.db_size
  storage_mb                    = 32768
  backup_retention_days         = 7
  administrator_login           = "chattie"
  administrator_password        = random_password.db.result
  delegated_subnet_id           = azurerm_subnet.db.id
  private_dns_zone_id           = azurerm_private_dns_zone.db.id
  public_network_access_enabled = false
  depends_on                    = [azurerm_private_dns_zone_virtual_network_link.db]
  lifecycle {
    ignore_changes = [zone] # Azure picks the zone
  }
}

resource "azurerm_postgresql_flexible_server_database" "main" {
  name      = "chattie"
  server_id = azurerm_postgresql_flexible_server.main.id
}

# ---------- VMs ----------

resource "random_password" "session" {
  length  = 48
  special = false
}

# Azure insists on a login key for Linux VMs. Port 22 is never opened; use
# "Run command" in the portal if you need a shell.
resource "tls_private_key" "ssh" {
  algorithm = "RSA"
  rsa_bits  = 4096
}

resource "azurerm_linux_virtual_machine" "redis" {
  name                  = "chattie-redis"
  resource_group_name   = azurerm_resource_group.main.name
  location              = azurerm_resource_group.main.location
  size                  = var.redis_size
  admin_username        = "chattie"
  network_interface_ids = [azurerm_network_interface.redis.id]
  custom_data           = filebase64("../vm/redis.sh")
  admin_ssh_key {
    username   = "chattie"
    public_key = tls_private_key.ssh.public_key_openssh
  }
  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "StandardSSD_LRS"
  }
  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-26_04-lts"
    sku       = "server"
    version   = "latest"
  }
  # The first-boot script needs internet access.
  depends_on = [
    azurerm_lb_outbound_rule.internet,
    azurerm_network_interface_backend_address_pool_association.outbound,
  ]
}

resource "azurerm_linux_virtual_machine" "app" {
  count                 = var.app_count
  name                  = "chattie-app-${count.index + 1}"
  resource_group_name   = azurerm_resource_group.main.name
  location              = azurerm_resource_group.main.location
  size                  = var.app_size
  admin_username        = "chattie"
  network_interface_ids = [azurerm_network_interface.app[count.index].id]
  # A different image changes the boot script, which replaces the VM.
  custom_data = base64encode(templatefile("../vm/app.sh.tftpl", {
    image          = var.image
    db_host        = azurerm_postgresql_flexible_server.main.fqdn
    db_password    = random_password.db.result
    db_pool        = 5 # connections per container; the smallest database allows about 35 in total
    redis_host     = local.redis_ip
    session_secret = random_password.session.result
  }))
  admin_ssh_key {
    username   = "chattie"
    public_key = tls_private_key.ssh.public_key_openssh
  }
  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "StandardSSD_LRS"
  }
  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-26_04-lts"
    sku       = "server"
    version   = "latest"
  }
  depends_on = [
    azurerm_lb_outbound_rule.internet,
    azurerm_network_interface_backend_address_pool_association.outbound,
  ]
}

output "url" {
  value = "http://${azurerm_public_ip.lb.fqdn}"
}
