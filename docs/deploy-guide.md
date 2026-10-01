# Deploying Chattie: the complete guide

Everything from an empty GitHub account to a running distributed chat system
on AWS and Azure, and back to zero cost. Follow it top to bottom. All commands
are for PowerShell on Windows, run from the repository root unless a step says
otherwise.

Contents:

1. [What you are building](#1-what-you-are-building)
2. [Install the tools](#2-install-the-tools)
3. [Put the code on GitHub and let it build](#3-put-the-code-on-github-and-let-it-build)
4. [Run it on your own machine](#4-run-it-on-your-own-machine)
5. [How Terraform works](#5-how-terraform-works)
6. [Deploy to AWS](#6-deploy-to-aws)
7. [Deploy to Azure](#7-deploy-to-azure)
8. [Experiments](#8-experiments)
9. [Deploy a new version](#9-deploy-a-new-version)
10. [Tear down](#10-tear-down)
11. [Troubleshooting](#11-troubleshooting)
12. [When you are finished for good](#12-when-you-are-finished-for-good)
13. [Reference](#13-reference)

---

## 1. What you are building

```text
                       +--> app VM 1: chattie + chattie-publisher --+
browser --> load       |                                            +--> data VM:
            balancer --+                                            |    postgres + redis
            (port 80)  +--> app VM 2: chattie + chattie-publisher --+
```

- **Load balancer.** The only thing reachable from the internet. It spreads
  browsers across the app VMs and skips any VM whose `/readyz` check fails.
- **App VMs.** Each runs two Docker containers from the same image: `chattie`
  (HTTP, WebSocket, web page) and `chattie-publisher` (forwards committed
  events from Postgres to Redis).
- **Data VM.** Runs the `postgres` and `redis` containers. Postgres holds
  every message. Redis only carries live notifications between app VMs.

How the image gets there:

```text
git push --> GitHub Actions: test, build image --> ghcr.io --> each app VM pulls it
```

Nothing is built on your machine. AWS and Azure are two separate copies of the
system, each with its own users and messages.

**Cost.** About 9 cents an hour on AWS and 8 cents an hour on Azure while the
system is up (approximate), paid from your credits. After `destroy` it costs
nothing.

**This is a learning setup, not production.**

- Plain HTTP, no HTTPS. Use throwaway passwords in the chat.
- The database lives on the data VM's disk and is deleted with it.
- Terraform keeps a state file on your machine that contains the generated
  passwords. It is git-ignored. Do not share it.

---

## 2. Install the tools

You need accounts on [GitHub](https://github.com), AWS and Azure, and these
tools:

```powershell
winget install Git.Git
winget install Docker.DockerDesktop
winget install GoLang.Go
winget install Hashicorp.Terraform
winget install Amazon.AWSCLI
winget install Microsoft.AzureCLI
```

Skip any you already have. **Close the terminal and open a new one**, then
check that each answers:

```powershell
git --version
docker --version
go version
terraform version
aws --version
az version
```

Start Docker Desktop and wait until it says it is running.

| Tool | What it is for |
| --- | --- |
| Git | Sends the code to GitHub |
| Docker Desktop | Runs the system on your machine |
| Go | Runs the tests against a running system |
| Terraform | Creates and deletes the cloud resources |
| AWS CLI, Azure CLI | Sign Terraform in to each cloud, and let you inspect resources |

---

## 3. Put the code on GitHub and let it build

### 3.1 Create the repository

1. Open <https://github.com/new>.
2. Owner `aniruddha81`, name `chattie-cloud`, visibility **Public**.
3. Do not add a README, .gitignore or license. Click **Create repository**.

The names matter: the image is published as
`ghcr.io/aniruddha81/chattie-cloud`. If you use other names, change the image
name in three places: the `image` variable in `deploy/aws/main.tf` and
`deploy/azure/main.tf`, and the `image:` line in `deploy/local/compose.yaml`.

### 3.2 Push the code

```powershell
git add .
git status
git commit -m "Chattie Cloud"
git branch -M main
git remote add origin https://github.com/aniruddha81/chattie-cloud.git
git push -u origin main
```

`git status` shows what will be committed. Make sure no `.tfstate` or
`.tfvars` file is listed. The first push opens a browser window to sign in to
GitHub.

### 3.3 Watch the build

Open the repository on GitHub and click the **Actions** tab. A run named `CI`
starts by itself. It has two jobs, defined in `.github/workflows/ci.yml`:

1. **test**: starts the whole system on GitHub's machine with Docker Compose
   and runs the integration tests against it, including one that stops Redis.
2. **image**: builds the Docker image and publishes it with two tags:
   `latest` and the first seven characters of the commit ID.

Wait for both jobs to turn green (about three minutes). Click the run to see
its summary, which shows the published image name and tag.

If a job fails, click it to read the log. The step named *Show container
logs* prints what the containers said.

### 3.4 Make the image public

A new image is private. The VMs pull it without logging in, so make it public
once:

1. Open <https://github.com/aniruddha81?tab=packages>.
2. Click `chattie-cloud`, then **Package settings** on the right.
3. At the bottom, **Change visibility**, choose **Public** and confirm.

Check that anyone can pull it:

```powershell
docker logout ghcr.io
docker pull ghcr.io/aniruddha81/chattie-cloud:latest
```

It should end with `Status: Downloaded newer image`.

---

## 4. Run it on your own machine

This is free and shows you what a healthy system looks like before you pay
for one.

```powershell
docker compose -f deploy/local/compose.yaml up -d
docker compose -f deploy/local/compose.yaml ps
```

All seven services should show `Up`: `proxy`, `app1`, `app2`, `app3`,
`publisher`, `postgres`, `redis`. Open <http://localhost:8080>, create two
accounts in two browser windows (use a private window for the second) and
chat. The sidebar shows which instance each window is connected to.

Run the tests against it:

```powershell
go test -p 1 -vet=off -tags integration ./tests/integration/
```

Stop it, keeping the data, or stop it and delete the data:

```powershell
docker compose -f deploy/local/compose.yaml down
docker compose -f deploy/local/compose.yaml down -v
```

---

## 5. How Terraform works

Terraform reads the `.tf` file in a folder, which describes what should
exist, and makes the cloud match it. There are five commands to know:

| Command | What it does |
| --- | --- |
| `terraform init` | Downloads the plugins for the cloud. Run once per folder. |
| `terraform plan` | Shows what would be created, changed or deleted. Changes nothing. |
| `terraform apply` | Shows the plan, asks for `yes`, then does it. |
| `terraform output` | Prints values such as the URL. |
| `terraform destroy` | Deletes everything this folder created. |

Terraform remembers what it created in `terraform.tfstate` inside the folder.
Do not delete that file while resources exist, or Terraform forgets them and
you have to delete them by hand in the console.

`-chdir=deploy/aws` tells Terraform which folder to work in, so you can stay
in the repository root.

---

## 6. Deploy to AWS

### 6.1 Create an access key

Terraform needs credentials to act as you.

1. Sign in to the [AWS console](https://console.aws.amazon.com) and open
   **IAM**.
2. **Users**, **Create user**. Name it `terraform`. Leave console access off.
3. **Attach policies directly**, tick `AdministratorAccess`, create the user.
4. Open the user, **Security credentials**, **Create access key**, choose
   **Command Line Interface**, confirm, create.
5. Keep the page open. It shows the access key ID and the secret access key.
   The secret is shown only once.

### 6.2 Sign in from the terminal

```powershell
aws configure
```

| Prompt | Enter |
| --- | --- |
| AWS Access Key ID | the key ID from the page |
| AWS Secret Access Key | the secret from the page |
| Default region name | `us-east-1` |
| Default output format | `json` |

Check it worked:

```powershell
aws sts get-caller-identity
```

It prints your account number and `user/terraform`. The key is stored in
`C:\Users\<you>\.aws\credentials`. Never put it in the repository.

### 6.3 Look before you create

```powershell
terraform -chdir=deploy/aws init
terraform -chdir=deploy/aws plan
```

`plan` ends with `Plan: 23 to add, 0 to change, 0 to destroy.` Nothing exists
yet. Section 13 lists what each of those resources is.

### 6.4 Create it

```powershell
terraform -chdir=deploy/aws apply
```

Type `yes` when asked. It takes about four minutes, most of it the load
balancer. It ends with:

```text
Apply complete! Resources: 23 added, 0 changed, 0 destroyed.

Outputs:

url = "http://chattie-123456789.us-east-1.elb.amazonaws.com"
```

**From this moment credit is being spent.** Section 10 stops it.

### 6.5 Wait for the VMs, then check

Terraform is finished when the VMs exist, but each VM still has to install
Docker and pull the image, which takes about three more minutes. Until then
the load balancer answers `502` or `503`.

Save the URL in a variable and ask who is ready:

```powershell
$url = terraform -chdir=deploy/aws output -raw url
curl.exe -s "$url/readyz"
```

When it is up you get:

```text
{"connections":0,"instance":"ip-10-0-0-123"}
```

Ask ten times and watch the `instance` change as the load balancer alternates
between the two VMs:

```powershell
1..10 | ForEach-Object { curl.exe -s "$url/readyz"; "" }
```

See what the load balancer thinks of each VM:

```powershell
$tg = aws elbv2 describe-target-groups --names chattie --query "TargetGroups[0].TargetGroupArn" --output text
aws elbv2 describe-target-health --target-group-arn $tg --query "TargetHealthDescriptions[].[Target.Id,TargetHealth.State]" --output table
```

Both should say `healthy`.

### 6.6 Use it

Open the URL in two browser windows, create two accounts and chat. Then run
the tests against the cloud:

```powershell
$env:CHATTIE_URLS = "$url,$url"
go test -p 1 -vet=off -tags integration -count=1 ./tests/integration/
Remove-Item Env:CHATTIE_URLS
```

### 6.7 Get a shell on a VM

List the VMs:

```powershell
aws ec2 describe-instances --filters "Name=tag:Name,Values=chattie-*" "Name=instance-state-name,Values=running" --query "Reservations[].Instances[].[Tags[?Key=='Name']|[0].Value,InstanceId,PrivateIpAddress]" --output table
```

There is no SSH. To open a shell: AWS console, **EC2**, **Instances**, select
a VM, **Connect**, **Session Manager** tab, **Connect**. A terminal opens in
the browser. If the tab says the instance is not available, wait two minutes
after boot and refresh.

Commands to run in that shell:

```bash
sudo docker ps                             # the containers on this VM
sudo docker logs -f chattie                # app log (Ctrl+C to stop)
sudo docker logs -f chattie-publisher      # publisher log
sudo cat /var/log/cloud-init-output.log    # what the first-boot script did
```

On the data VM, look inside the database:

```bash
sudo docker exec -it postgres psql -U chattie -c "SELECT id, room_id, sequence, content FROM messages ORDER BY id DESC LIMIT 10"
sudo docker exec -it postgres psql -U chattie -c "SELECT count(*) FILTER (WHERE published_at IS NULL) AS pending, count(*) AS total FROM outbox"
```

### 6.8 Check your credit

AWS console, **Billing and Cost Management**, **Credits**. Charges appear
with a delay of several hours.

---

## 7. Deploy to Azure

The Azure credit expires first, so do not leave this part too late.

### 7.1 Sign in

```powershell
az login
az account show --query "{name:name, id:id, state:state}" -o table
```

A browser window opens to sign in. The table shows the subscription Terraform
will use.

### 7.2 Choose a region and VM sizes you are allowed to use

Student subscriptions allow only a few regions. This usually lists them:

```powershell
az policy assignment list --query "[].parameters.listOfAllowedLocations.value" -o tsv
```

If it prints nothing, there is no restriction, and `eastus` is fine. Then
check that the two VM sizes the deployment uses are available in your region
(replace `eastus` if needed):

```powershell
az vm list-skus --location eastus --size Standard_B1 --all --query "[].{size:name, blocked:restrictions[0].reasonCode}" -o table
```

`Standard_B1s` and `Standard_B1ms` should appear with nothing in the
`blocked` column. If one is blocked, pick another size from the list and set
it in the next step.

### 7.3 Write your settings

```powershell
$id = az account show --query id -o tsv
Set-Content deploy/azure/terraform.tfvars -Encoding ascii -Value "subscription_id = `"$id`"", "location        = `"eastus`""
Get-Content deploy/azure/terraform.tfvars
```

The file should look like this. It is git-ignored.

```hcl
subscription_id = "00000000-0000-0000-0000-000000000000"
location        = "eastus"
```

To use other VM sizes, add lines such as `app_size = "Standard_B2s"` or
`data_size = "Standard_B2s"`.

### 7.4 Create it

```powershell
terraform -chdir=deploy/azure init
terraform -chdir=deploy/azure plan
terraform -chdir=deploy/azure apply
```

`plan` ends with `Plan: 27 to add`. Type `yes` for `apply`. It takes about
five minutes and prints:

```text
url = "http://chattie-ab12cd.eastus.cloudapp.azure.com"
```

**From this moment credit is being spent.**

### 7.5 Wait for the VMs, then check

As on AWS, the VMs need about three minutes to install Docker and pull the
image.

```powershell
$url = terraform -chdir=deploy/azure output -raw url
curl.exe -s "$url/readyz"
1..10 | ForEach-Object { curl.exe -s "$url/readyz"; "" }
```

The `instance` is `chattie-app-1` or `chattie-app-2`. List the VMs:

```powershell
az vm list -g chattie -d --query "[].{name:name, state:powerState, ip:privateIps}" -o table
```

### 7.6 Use it

Open the URL in two browser windows and chat, then run the tests:

```powershell
$env:CHATTIE_URLS = "$url,$url"
go test -p 1 -vet=off -tags integration -count=1 ./tests/integration/
Remove-Item Env:CHATTIE_URLS
```

### 7.7 Run commands on a VM

There is no SSH. Azure can run a command on a VM for you. Define this helper
once per terminal:

```powershell
function onvm($name, $command) {
    az vm run-command invoke -g chattie -n $name --command-id RunShellScript --scripts $command --query "value[0].message" -o tsv
}
```

Then (each call takes about 30 seconds; commands run as root, so no `sudo`):

```powershell
onvm chattie-app-1 "docker ps"
onvm chattie-app-1 "docker logs --tail 20 chattie"
onvm chattie-app-1 "tail -n 30 /var/log/cloud-init-output.log"
onvm chattie-data  "docker exec postgres psql -U chattie -c 'SELECT id, room_id, sequence, content FROM messages ORDER BY id DESC LIMIT 10'"
```

### 7.8 Check your credit

Azure portal, **Subscriptions**, your subscription. The overview shows the
remaining credit and the cost so far, with a delay of several hours.

---

## 8. Experiments

This is the point of the project. Keep two browser windows open on the chat,
signed in as two users, ideally connected to different instances (the sidebar
shows which; reload a window until they differ).

The commands below are the ones to run **on a VM**. On AWS, type them in a
Session Manager shell with `sudo` in front. On Azure, wrap them:
`onvm chattie-app-1 "docker stop chattie"`.

### 8.1 Messages cross VMs

Send a message from each window. Both arrive, although the users are on
different VMs. The path was: VM 1 saved the message in Postgres, the
publisher announced it on Redis, VM 2 heard it and pushed it to its browser.

### 8.2 Stop a chat container

On app VM 1:

```bash
docker stop chattie
```

The window connected to that VM shows *reconnecting*, then connects to the
other VM and still has every message. Before stopping, the container told its
browsers to reconnect and waited ten seconds. On AWS, run the target-health
command from 6.5 and see the VM turn `unhealthy`. Bring it back:

```bash
docker start chattie
```

### 8.3 Stop the publishers

On **both** app VMs:

```bash
docker stop chattie-publisher
```

Send a message. The sender sees it, because it was saved in Postgres and
acknowledged. The other user does not, because nobody is forwarding events to
Redis. On the data VM, count the waiting events:

```bash
docker exec postgres psql -U chattie -c "SELECT count(*) FROM outbox WHERE published_at IS NULL"
```

Start one publisher:

```bash
docker start chattie-publisher
```

The message appears in the other window within a second or two, and the count
returns to zero. This is the outbox pattern: an event can be delayed by a
failure, but not lost.

### 8.4 Stop Redis

On the data VM:

```bash
docker stop redis
```

Send a few messages. They are saved and acknowledged, but not delivered live.
Then:

```bash
docker start redis
```

The other window receives a `resync` notice and fetches everything it missed
from Postgres, without reloading the page.

### 8.5 Lose a whole VM

AWS (use an instance ID from the list in 6.7):

```powershell
aws ec2 terminate-instances --instance-ids i-0123456789abcdef0
```

Azure:

```powershell
az vm deallocate -g chattie -n chattie-app-2
```

The chat keeps working on the remaining VM. To repair it: on AWS,
`terraform -chdir=deploy/aws apply` creates a replacement; on Azure,
`az vm start -g chattie -n chattie-app-2`.

### 8.6 Scale out

```powershell
terraform -chdir=deploy/aws apply -var app_count=3
```

Terraform adds one VM and registers it with the load balancer. After about
three minutes, `/readyz` shows three instance names. A later `apply` without
`-var` goes back to two. To keep three, put `app_count = 3` in
`deploy/aws/terraform.tfvars` (or the Azure one).

### 8.7 Stop Postgres

On the data VM:

```bash
docker stop postgres
```

Now sending fails and both app VMs fail `/readyz`, so the load balancer has
nowhere to send traffic. Postgres is the one part that everything depends on.
Start it again:

```bash
docker start postgres
```

---

## 9. Deploy a new version

1. Change the code, then push:

   ```powershell
   git add .
   git commit -m "Describe the change"
   git push
   ```

2. Wait for the `CI` run on GitHub to turn green.
3. Find the image tag. It is the first seven characters of the commit ID:

   ```powershell
   git rev-parse --short=7 HEAD
   ```

4. Apply with that tag:

   ```powershell
   terraform -chdir=deploy/aws apply -var image=ghcr.io/aniruddha81/chattie-cloud:abc1234
   ```

Terraform replaces the app VMs with new ones that pull the new image. They are
replaced together, so the chat is unreachable for a few minutes. Messages are
safe in Postgres on the data VM, which is not touched.

A later `apply` without `-var image=...` would go back to `latest`. To make a
version stick, put it in `terraform.tfvars`:

```hcl
image = "ghcr.io/aniruddha81/chattie-cloud:abc1234"
```

Terraform still runs from your machine. It builds nothing; it only tells the
cloud what should exist.

---

## 10. Tear down

Do this whenever you stop for the day. Bringing it back later is one `apply`.

```powershell
terraform -chdir=deploy/aws destroy
terraform -chdir=deploy/azure destroy
```

Type `yes`. Everything is deleted, including the database.

Confirm nothing is left on AWS. Both commands should print `[]`:

```powershell
aws ec2 describe-instances --filters "Name=tag:Name,Values=chattie-*" "Name=instance-state-name,Values=pending,running,stopping,stopped" --query "Reservations[].Instances[].InstanceId"
aws elbv2 describe-load-balancers --query "LoadBalancers[].LoadBalancerName"
```

Confirm nothing is left on Azure. This should print `false`:

```powershell
az group exists --name chattie
```

Azure may keep a group named `NetworkWatcherRG`. It is created by Azure
itself and costs nothing.

Look at the billing page of each cloud the next day to confirm the cost
stopped growing.

**About the credits.** Left running all day, this system would use the AWS
credit in about 90 days and the Azure credit in about 43. Neither cloud
charges a card when credit runs out: AWS closes a free-plan account and Azure
disables the subscription. Destroy when you are not using it and the credits
will easily outlast your learning.

---

## 11. Troubleshooting

| What you see | Likely cause and fix |
| --- | --- |
| `terraform`, `aws` or `az` is not recognized | Open a new terminal after installing. |
| `git push` is rejected | The GitHub repository was created with a README. Create it empty, or run `git pull --rebase origin main` first. |
| The `CI` run fails in *Integration and failure tests* | Read the *Show container logs* step. Run the same tests locally (section 4) to reproduce. |
| The `image` job fails with `denied` | Usually a package named `chattie-cloud` already exists under your account from another repository. Delete it under **Packages**, then re-run the job. |
| `502` or `503` for more than six minutes after `apply` | The first-boot script failed. Read `/var/log/cloud-init-output.log` on an app VM (6.7 or 7.7). |
| That log ends with `docker pull` and `denied` or `unauthorized` | The image is still private. Do 3.4, then replace the VMs (see below). |
| That log ends with `manifest unknown` | The image tag does not exist. Check the tag in the Actions run summary. |
| `/readyz` says `postgres unavailable` | The data VM is still starting, or its Postgres container stopped. Check `docker ps` on the data VM. |
| AWS: `VcpuLimitExceeded` | Your account allows fewer VMs. Use `-var app_count=1`. |
| AWS: `InvalidClientTokenId` or `AuthFailure` | The access key is wrong. Run `aws configure` again. |
| Azure: `RequestDisallowedByAzure` or `not allowed by policy` | The region is not allowed for your subscription. Change `location` (7.2). |
| Azure: `SkuNotAvailable` | The VM size is not offered there. Set `app_size` or `data_size` (7.3). |
| Azure: `apply` is slow the first time | Terraform is registering resource providers on a new subscription. Let it finish. |
| `Error acquiring the state lock` | A previous Terraform run was interrupted. Run `terraform -chdir=<folder> force-unlock <ID>` with the ID from the message. |
| Session Manager lists no instances | The VM's agent needs two minutes after boot. Refresh. |

To replace the app VMs after fixing something (for example after making the
image public), force Terraform to recreate them:

```powershell
terraform -chdir=deploy/aws apply -replace="aws_instance.app[0]" -replace="aws_instance.app[1]"
terraform -chdir=deploy/azure apply -replace="azurerm_linux_virtual_machine.app[0]" -replace="azurerm_linux_virtual_machine.app[1]"
```

If a `destroy` fails halfway, run it again. It continues where it stopped.

---

## 12. When you are finished for good

1. Destroy both clouds and confirm, as in section 10.
2. Delete the AWS access key: IAM, **Users**, `terraform`, **Security
   credentials**, delete the key. Then remove it from your machine:

   ```powershell
   Remove-Item $HOME\.aws\credentials
   ```

3. Sign out of Azure:

   ```powershell
   az logout
   ```

4. Stop the local stack and delete its data:

   ```powershell
   docker compose -f deploy/local/compose.yaml down -v
   ```

---

## 13. Reference

### What Terraform creates on AWS (`deploy/aws/main.tf`)

| Resource | Purpose |
| --- | --- |
| VPC, internet gateway, 2 subnets, route table | A private network in two zones with a way out to the internet |
| 3 security groups | Firewalls. Load balancer: port 80 from anyone. App: 8080 from the load balancer only. Data: 5432 and 6379 from the app VMs only |
| IAM role and instance profile | Lets you open a Session Manager shell on the VMs |
| 2 random passwords | The database password and the secret that signs login cookies |
| 1 data VM (`t3.small`) | Postgres and Redis containers |
| 2 app VMs (`t3.micro`) | `chattie` and `chattie-publisher` containers |
| Load balancer, target group, listener | Receives port 80 and forwards to healthy app VMs on 8080 |

### What Terraform creates on Azure (`deploy/azure/main.tf`)

| Resource | Purpose |
| --- | --- |
| Resource group | A folder that holds everything, so it can be deleted together |
| Virtual network, subnet | The private network |
| Network security group | Firewall. Only port 8080 is open to the internet; Postgres and Redis are reachable only inside the network |
| Public IP with a DNS name | The address of the load balancer |
| Load balancer, 2 pools, probe, rule, outbound rule | Forwards port 80 to healthy app VMs on 8080, and gives the VMs a way out to the internet |
| 3 network interfaces | One per VM. The data VM has the fixed address `10.1.0.10` |
| 2 random passwords, 1 key | Database password, cookie secret, and the login key Azure requires (port 22 is never opened) |
| 1 data VM (`Standard_B1ms`) | Postgres and Redis containers |
| 2 app VMs (`Standard_B1s`) | `chattie` and `chattie-publisher` containers |

### What a VM does on first boot (`deploy/vm`)

- `app.sh.tftpl`: installs Docker, pulls the image, writes the settings to
  `/etc/chattie.env`, starts the two containers with `--restart always`.
- `data.sh.tftpl`: installs Docker, starts Postgres and Redis.

Terraform fills in the image name, addresses and passwords before sending the
script to the VM.

### Settings you can change

| Variable | Default | Where |
| --- | --- | --- |
| `image` | `ghcr.io/aniruddha81/chattie-cloud:latest` | both |
| `app_count` | `2` | both |
| `region` | `us-east-1` | AWS |
| `subscription_id` | none, required | Azure |
| `location` | `eastus` | Azure |
| `app_size`, `data_size` | `Standard_B1s`, `Standard_B1ms` | Azure |

Set them with `-var name=value` for one run, or in `terraform.tfvars` inside
the folder to keep them.

### Command summary

```powershell
# local
docker compose -f deploy/local/compose.yaml up -d
docker compose -f deploy/local/compose.yaml down

# AWS
terraform -chdir=deploy/aws init
terraform -chdir=deploy/aws apply
terraform -chdir=deploy/aws output -raw url
terraform -chdir=deploy/aws destroy

# Azure
terraform -chdir=deploy/azure init
terraform -chdir=deploy/azure apply
terraform -chdir=deploy/azure output -raw url
terraform -chdir=deploy/azure destroy

# tests against any running system
$env:CHATTIE_URLS = "$url,$url"
go test -p 1 -vet=off -tags integration -count=1 ./tests/integration/
```
