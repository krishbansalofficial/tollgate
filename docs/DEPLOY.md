# Deploying Tollgate

Target: a single-node k3s cluster managed by Argo CD from this repo. Current host: **Azure for Students** (ARM64, 2 vCPU / 4 GB, paid from student credit, see [the Azure section](#alternative-host-azure-for-students-github-student-pack-no-card)). Oracle Always Free (steps 1–2 below) and Hetzner use the same node setup; from step 3 on, everything is identical.

```mermaid
flowchart LR
    dev[git push main] --> ci[GitHub Actions<br/>lint · test -race · tf validate]
    ci --> rel[build+push once<br/>Trivy + SBOM on the digest]
    rel --> bump[pin digest<br/>deploy/tollgate]
    bump --> argo[Argo CD on k3s<br/>watches deploy/]
    argo --> k3s[(k3s VM)]
```

After the one-time setup below, **every merge to `main` deploys itself**. CI builds the multi-arch image once and pushes `ghcr.io/krishbansalofficial/tollgate:sha-<commit>`. Trivy and the SBOM then run against that exact published digest, for both architectures. Only then does CI pin the digest in `deploy/tollgate/kustomization.yaml`, on the bot-owned `deploy` branch rather than `main`: that branch is exactly the built commit plus one pin commit, and only moves forward. Argo CD's tollgate app tracks it, so `main` never gets bot commits and you never have to pull after a deploy. A migration Job (Argo CD Sync hook, wave 1) runs after the secrets and Postgres are healthy and before the new pods (wave 2).

## What runs on the node

| Namespace | Workload | Memory limit |
|---|---|---|
| tollgate | tollgate ×2, Postgres, Redis | 2×256 Mi + 512 Mi + 160 Mi |
| observability | OTel Collector, Tempo, Prometheus, Grafana | 256 + 512 + 768 + 256 Mi |
| argocd | Argo CD | ~1 Gi |
| kube-system / cert-manager | k3s, Traefik, CoreDNS, sealed-secrets, cert-manager | ~1 Gi |

That's about 4.5 GB at the limits. Actual use is well below the limits; on a 4 GB node (Azure, Hetzner) the limits are overcommitted, so watch `kubectl top pods` after the first deploy.

## One-time setup (about 1 hour)

### 0. Tools

`terraform` (≥ 1.9), `kubectl`, `kubeseal` (≥ 0.41), `ssh`, `openssl`. On Windows: `winget install Hashicorp.Terraform Kubernetes.kubectl`; for kubeseal, download the Windows binary from its GitHub release.

### 1. Oracle Cloud account and API key

1. Sign up at cloud.oracle.com. A card is required for verification; Always Free resources are not charged. **Do not upgrade to Pay As You Go** unless you mean to.
2. Profile → My profile → **API keys** → Add API key → download the private key to `~/.oci/oci_api_key.pem`.
3. Copy the shown tenancy/user OCIDs, fingerprint and region into `infra/terraform/envs/oracle/terraform.tfvars` (start from `terraform.tfvars.example`; the real file is gitignored).

### 2. Provision the VM

```bash
cd infra/terraform/envs/oracle
terraform init
terraform apply          # prints public_ip and an ssh command
```

The node module **refuses** more than 2 OCPU / 12 GB / 200 GB. Exceeding the Always Free allowance on a free tenancy disables and then deletes all A1 instances.

If apply fails with `Out of host capacity`, A1 capacity in that availability domain is exhausted. Retry later or set `availability_domain_index = 1` (or 2).

cloud-init installs k3s and opens 80/443 in the host firewall. Oracle's Ubuntu images block those ports at the iptables level even when the security list allows them. The Kubernetes API (6443) is never exposed; everything goes over SSH.

### 3. Repo access for Argo CD

Nothing to do: the repo is public, so Argo CD reads it over HTTPS with no credentials. (For a private fork, register it in Argo CD with a read-only deploy key.)

### 4. DNS: free DuckDNS name

1. Sign in at duckdns.org and create a subdomain, e.g. `mytollgate`, pointing at the VM's `public_ip`.
2. Replace `change-me` with your subdomain in `deploy/tollgate/tollgate.yaml` and `deploy/observability/k8s/grafana.yaml`. Grafana uses `grafana.<name>.duckdns.org`, which DuckDNS resolves via its wildcard.
3. Commit and push.

The ingresses use the `letsencrypt-prod` issuer. If you're iterating on DNS, switch to `letsencrypt-staging` first to avoid Let's Encrypt rate limits.

### 5. Bootstrap Argo CD

```bash
infra/bootstrap.sh ubuntu@<public_ip>
```

This installs Argo CD and applies `deploy/argocd/root.yaml`. Argo then installs sealed-secrets, cert-manager, the issuers, observability and Tollgate, in sync-wave order.

### 6. Secrets (Sealed Secrets)

```bash
scripts/seal-secrets.sh ubuntu@<public_ip>
git add deploy/*/sealed-secrets.yaml deploy/*/kustomization.yaml
git commit -m "deploy: sealed secrets" && git push
```

The script prompts with hidden input for provider keys, a GitHub token with `read:packages` (for pulling the private image), and a Grafana password. It generates a random Postgres password and encrypts everything with the cluster's public key.

Only the encrypted files are committed: plaintext never touches disk or git. The script refuses to overwrite existing secrets, because a new Postgres password would lock the app out of the existing database. Use `FORCE=1` only for a fresh database.

### 7. First key and smoke test

```bash
ssh ubuntu@<ip> sudo k3s kubectl -n tollgate exec deploy/tollgate -- /usr/local/bin/tollgate keys create --project myapp
curl https://<name>.duckdns.org/v1/chat/completions \
  -H "Authorization: Bearer tg_..." \
  -d '{"model":"tollgate/cheapest","messages":[{"role":"user","content":"hello"}]}'
```

Grafana is at `https://grafana.<name>.duckdns.org` (user `admin`). Argo CD is reached through an SSH tunnel; `bootstrap.sh` prints the command.

## Ollama on the laptop (last-resort fallback)

The cluster reaches the laptop's Ollama over **Tailscale**, so the laptop never opens a port to the internet.

1. Install Tailscale on the laptop and on the node (`curl -fsSL https://tailscale.com/install.sh | sh && sudo tailscale up`), both in your tailnet.
2. On the laptop: `OLLAMA_HOST=0.0.0.0 ollama serve` and `ollama pull llama3.2:3b`. An 8 GB RTX 4070 runs a 3B model comfortably.
3. In `deploy/tollgate/kustomization.yaml` set:
   - `TOLLGATE_OLLAMA_BASE_URL=http://<laptop-tailnet-name>:11434/v1`
   - `TOLLGATE_FALLBACK_MODEL=ollama/llama3.2:3b`
4. Push.

When the laptop sleeps, Ollama's circuit breaker opens and the fallback fails fast instead of timing out.

## Operations

| Task | How |
|---|---|
| Deploy | merge to `main` |
| Roll back | `git revert` the `deploy: tollgate sha-…` commit |
| Revoke a key | `tollgate keys revoke --prefix tg_XXXXXXXX` (takes effect within the 30 s key-cache TTL) |
| Logs | `sudo k3s kubectl -n tollgate logs deploy/tollgate` (JSON, with `trace_id`) |
| Trace a request | paste its `X-Request-ID` into Grafana → Explore → Tempo |
| Alerts | Prometheus `/alerts` and Grafana; no Alertmanager, to keep $0 |

## Alternative host: Azure for Students (GitHub Student Pack, no card)

`infra/terraform/envs/azure` builds a `Standard_B2pls_v2` VM (2 vCPU, 4 GB, ARM64, ~$25/month) with a static IP and an NSG (SSH from one CIDR, 80/443 open), using the same cloud-init module. The $100 credit covers about 4 months and renews yearly while you are a student. With no card on file, running out disables the subscription instead of billing you.

1. Claim the Azure offer from the Student Pack page ("connect your GitHub account on Microsoft Azure").
2. `winget install Microsoft.AzureCLI`, then `az login`. Terraform uses this login; no key goes in a file.
3. `cp terraform.tfvars.example terraform.tfvars` in that directory and fill it in. `subscription_id` comes from `az account show --query id -o tsv`.
4. `terraform init && terraform apply`, then steps 3–7 above with `ubuntu@<ip>`.

If apply fails with `RequestDisallowedByPolicy`, the region isn't allowed for student subscriptions; change `location`. If it fails with `SkuNotAvailable`, list allowed sizes with `az vm list-skus -l <region> --resource-type virtualMachines` (entries with empty `restrictions`).

## Fallback host: Hetzner (≈ €4/month)

If Oracle has no A1 capacity or changes the free tier again, `infra/terraform/envs/hetzner` builds a CAX11 (2 vCPU ARM64, 4 GB) with **the same cloud-init module**. Steps 3–7 are identical; use `root@<ip>` instead of `ubuntu@<ip>`. 4 GB fits the stack; Argo CD and Prometheus are the largest consumers.

An AWS variant would add `envs/aws` with VPC and EC2 resources and reuse `modules/k3s-cloud-init` unchanged.

## Teardown

```bash
cd infra/terraform/envs/oracle && terraform destroy
```

## Verified before first deploy

The manifests were applied to a local k3s v1.37.2 cluster (Docker) with the CI-built image:
- **Pods:** all became Ready under the pod security levels and NetworkPolicies.
- **Migrations:** applied via the Job.
- **Allowed request:** a request from the ingress namespace returned 200 through the cache, router and provider, and wrote a ledger row.
- **Blocked request:** a request from another namespace was rejected by NetworkPolicy.
- **Metrics:** Prometheus discovered both gateway pods.

That test surfaced two bugs, both fixed:
- **Invalid hostnames:** uppercase placeholder hostnames are invalid in Kubernetes.
- **Migration race:** the migration Job could lose a race with the NetworkPolicy controller on new pods. `tollgate migrate` now waits for the database with backoff.
