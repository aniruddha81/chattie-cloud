# Chattie on AWS: a load balancer, app VMs and one data VM.
#
#   internet -> load balancer :80 -> app VMs :8080 -> data VM (Postgres, Redis)
#
# A learning setup: plain HTTP, and the database lives on a VM disk.

terraform {
  required_providers {
    aws    = { source = "hashicorp/aws", version = ">= 6.0" }
    random = { source = "hashicorp/random", version = ">= 3.0" }
  }
}

provider "aws" {
  region = var.region
}

variable "region" {
  default = "us-east-1"
}

variable "app_count" {
  description = "How many chat VMs to run."
  default     = 2
}

variable "image" {
  description = "The image GitHub Actions published. Use a commit tag instead of latest to deploy a specific version."
  default     = "ghcr.io/aniruddha81/chattie-cloud:latest"
}

# ---------- network ----------

data "aws_availability_zones" "available" {
  state = "available"
}

resource "aws_vpc" "main" {
  cidr_block = "10.0.0.0/16"
  tags       = { Name = "chattie" }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
}

# Two subnets in two zones: the load balancer requires it, and the app VMs
# are spread across them.
resource "aws_subnet" "public" {
  count                   = 2
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.0.${count.index}.0/24"
  availability_zone       = data.aws_availability_zones.available.names[count.index]
  map_public_ip_on_launch = true
  tags                    = { Name = "chattie-${count.index + 1}" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }
}

resource "aws_route_table_association" "public" {
  count          = 2
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# ---------- firewall ----------
# Each layer only accepts traffic from the layer in front of it.

resource "aws_security_group" "lb" {
  name   = "chattie-lb"
  vpc_id = aws_vpc.main.id
  ingress {
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "app" {
  name   = "chattie-app"
  vpc_id = aws_vpc.main.id
  ingress {
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.lb.id]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "data" {
  name   = "chattie-data"
  vpc_id = aws_vpc.main.id
  ingress {
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.app.id]
  }
  ingress {
    from_port       = 6379
    to_port         = 6379
    protocol        = "tcp"
    security_groups = [aws_security_group.app.id]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# ---------- VM identity ----------

# Lets you open a shell on a VM from the AWS console (Session Manager)
# without SSH.
resource "aws_iam_role" "vm" {
  name = "chattie-vm"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "shell" {
  role       = aws_iam_role.vm.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_instance_profile" "vm" {
  name = "chattie-vm"
  role = aws_iam_role.vm.name
}

# ---------- VMs ----------

data "aws_ssm_parameter" "ubuntu" {
  name = "/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id"
}

resource "random_password" "db" {
  length  = 32
  special = false
}

resource "random_password" "session" {
  length  = 48
  special = false
}

resource "aws_instance" "data" {
  ami                    = data.aws_ssm_parameter.ubuntu.insecure_value
  instance_type          = "t3.small"
  subnet_id              = aws_subnet.public[0].id
  vpc_security_group_ids = [aws_security_group.data.id]
  iam_instance_profile   = aws_iam_instance_profile.vm.name
  user_data              = templatefile("../vm/data.sh.tftpl", { db_password = random_password.db.result })
  tags                   = { Name = "chattie-data" }
}

resource "aws_instance" "app" {
  count                  = var.app_count
  ami                    = data.aws_ssm_parameter.ubuntu.insecure_value
  instance_type          = "t3.micro"
  subnet_id              = aws_subnet.public[count.index % 2].id
  vpc_security_group_ids = [aws_security_group.app.id]
  iam_instance_profile   = aws_iam_instance_profile.vm.name
  # A different image changes the boot script, which replaces the VM.
  user_data_replace_on_change = true
  user_data = templatefile("../vm/app.sh.tftpl", {
    image          = var.image
    database_url   = "postgres://chattie:${random_password.db.result}@${aws_instance.data.private_ip}:5432/chattie?sslmode=disable&pool_max_conns=10"
    redis_url      = "redis://${aws_instance.data.private_ip}:6379"
    session_secret = random_password.session.result
  })
  tags = { Name = "chattie-app-${count.index + 1}" }
}

# ---------- load balancer ----------

resource "aws_lb" "main" {
  name               = "chattie"
  load_balancer_type = "application"
  security_groups    = [aws_security_group.lb.id]
  subnets            = aws_subnet.public[*].id
}

# The load balancer only sends traffic to VMs whose /readyz answers 200.
resource "aws_lb_target_group" "app" {
  name                 = "chattie"
  port                 = 8080
  protocol             = "HTTP"
  vpc_id               = aws_vpc.main.id
  deregistration_delay = 15
  health_check {
    path                = "/readyz"
    interval            = 10
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 2
  }
}

resource "aws_lb_target_group_attachment" "app" {
  count            = var.app_count
  target_group_arn = aws_lb_target_group.app.arn
  target_id        = aws_instance.app[count.index].id
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.main.arn
  port              = 80
  protocol          = "HTTP"
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.app.arn
  }
}

output "url" {
  value = "http://${aws_lb.main.dns_name}"
}
