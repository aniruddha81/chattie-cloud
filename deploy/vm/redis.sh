#!/bin/bash
# First-boot script for the Redis VM. Redis holds nothing durable here: only
# live notifications between app VMs and who is online. The firewall lets
# only the app VMs reach it.
# Log: /var/log/cloud-init-output.log
set -euxo pipefail

apt-get update
apt-get -o DPkg::Lock::Timeout=300 install -y docker.io

docker run -d --name redis --restart always -p 6379:6379 redis:8-alpine
