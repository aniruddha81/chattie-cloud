# Deploying Chattie to AWS and Azure

A simple deployment for learning: bring it up, experiment, tear it down.

```
internet -> load balancer :80 -> 2 app VMs :8080 -> 1 data VM (Postgres + Redis)
```

Everything runs in Docker. Each app VM runs two containers from the same
`chattie` image you use locally: the chat instance and the outbox publisher.
The data VM runs the Postgres and Redis containers. The two clouds are
separate environments with their own users and messages.

Nothing is built on your machine. GitHub Actions tests each push to `main`,
builds the Docker image and publishes it to GitHub's container registry
(`ghcr.io`). Each app VM pulls that image when it first boots.

```
git push -> GitHub Actions: test, build image -> ghcr.io -> VMs pull it
```

**Cost while running:** about 9 cents an hour on AWS and 8 cents an hour on
Azure (approximate; about $2 a day each). Nothing bills after `destroy`.

**This setup is for learning, not production:**

- Plain HTTP, no HTTPS. Use throwaway passwords.
- The database lives on the data VM's disk and is deleted with it.
- Terraform's state file on your machine contains the generated passwords. It
  is git-ignored; do not share it.

## One-time setup

### Put the code on GitHub

1. On GitHub, create an empty **public** repository named `chattie-cloud`
   under the `aniruddha81` account.
2. Push the code:

   ```
   git add .
   git commit -m "Chattie Cloud"
   git remote add origin https://github.com/aniruddha81/chattie-cloud.git
   git push -u origin main
   ```

3. Open the repository's **Actions** tab and wait for the `CI` run to turn
   green. Its summary shows the image it published.
4. Make the image downloadable without a login, so the VMs can pull it: your
   GitHub profile, **Packages**, `chattie-cloud`, **Package settings**,
   **Change visibility**, **Public**. This is needed once.

If your account or repository name is different, the image name changes too.
Set it in the `image` variable of both Terraform folders and in
`deploy/local/compose.yaml`.

### Install the tools

In PowerShell, then open a new terminal:

```
winget install Hashicorp.Terraform Amazon.AWSCLI Microsoft.AzureCLI
```

### Sign in to the clouds

**AWS sign-in.** In the AWS console: IAM, Users, Create user (for example
`terraform`), attach the `AdministratorAccess` policy, then Security
credentials, Create access key. Then:

```
aws configure
```

Enter the access key, the secret key, and region `us-east-1`.

**Azure sign-in.**

```
az login
az account show --query id -o tsv
```

Create `deploy/azure/terraform.tfvars` with that ID:

```
subscription_id = "00000000-0000-0000-0000-000000000000"
location        = "eastus"
```

Student subscriptions may only use a few regions. If `apply` says the location
is not allowed, use one the portal offers on its Create VM page.

## Deploy

Run from the repository root.

```
terraform -chdir=deploy/aws init
terraform -chdir=deploy/aws apply
```

Terraform lists what it will create and asks for `yes`. When it finishes it
prints the `url`. The VMs need about three minutes after that to install and
start; until then the load balancer answers 502 or 503.

Azure is the same with `deploy/azure`:

```
terraform -chdir=deploy/azure init
terraform -chdir=deploy/azure apply
```

## Check that it works

```
curl.exe http://<url>/readyz
```

Run it a few times: the `instance` name changes as the load balancer moves
between VMs. Then open the URL in two browser windows, create two accounts,
and chat. The sidebar shows which VM each window is connected to.

The integration tests also run against the cloud:

```
$env:CHATTIE_URLS = "http://<url>,http://<url>"
go test -tags integration ./tests/integration/
```

## Things to try

These are the distributed-systems lessons the project is built around.

1. **Messages cross VMs.** Two browsers on different instances still see each
   other's messages. Redis carries the event between VMs.
2. **Stop a chat container.** On one app VM run `sudo docker stop chattie`.
   Its browsers reconnect to the other VM and catch up from Postgres.
   `sudo docker start chattie` and the load balancer brings it back.
3. **Stop the publisher on both app VMs** (`sudo docker stop chattie-publisher`)
   and send a message. The sender sees it at once (it is committed), other
   users do not get it live. Start one publisher: the message arrives. That is
   the outbox at work.
4. **Stop Redis.** On the data VM run `sudo docker stop redis`, send messages,
   then `sudo docker start redis`. Nothing is lost: clients get a `resync`
   notice and fetch what they missed.
5. **Terminate an app VM** from the console. The other keeps serving.
   `terraform apply` creates a replacement.
6. **Scale out.** `terraform -chdir=deploy/aws apply -var app_count=3`

Getting a shell on a VM:

- AWS: console, EC2, select the instance, Connect, Session Manager.
- Azure: portal, the VM, Run command, RunShellScript.

Useful commands on a VM:

```
sudo docker ps                               # what is running
sudo docker logs -f chattie                  # app logs
sudo docker logs -f chattie-publisher        # publisher logs
sudo cat /var/log/cloud-init-output.log      # first-boot script output
```

## Deploy a new version

1. Push your change to `main` and wait for the `CI` run to turn green.
2. Copy the image tag from the run's summary. It is the first seven characters
   of the commit ID, for example `abc1234`.
3. Apply with that tag:

   ```
   terraform -chdir=deploy/aws apply -var image=ghcr.io/aniruddha81/chattie-cloud:abc1234
   ```

A different image replaces the app VMs. They are all replaced together, so the
chat is unreachable for a few minutes. Messages are safe in Postgres on the
data VM, which is not touched.

Terraform still runs from your machine. It builds nothing; it only tells the
cloud what to create.

If the app never comes up, look at `/var/log/cloud-init-output.log` on an app
VM. A failed `docker pull` usually means the image is still private (step 4 of
the GitHub setup) or the tag does not exist.

## Tear down

```
terraform -chdir=deploy/aws destroy
terraform -chdir=deploy/azure destroy
```

This deletes everything, including the database. Check the billing page the
next day to confirm spending stopped.

## About the credits

- AWS free plan: when the $200 runs out or six months pass, the account
  closes. It does not charge a card.
- Azure: when the credit runs out or expires, the subscription is disabled.
- Left running all day, this setup would use the AWS credit in about 90 days
  and the Azure credit in about 43. Destroy it when you are not using it and
  the credits will far outlast your learning.
- Azure's credit expires first, so try Azure before the two months are up.
