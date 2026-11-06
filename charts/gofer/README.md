# Gofer Helm chart

Single-replica Helm chart for [Gofer](https://github.com/cristianadrielbraun/gofer). Gofer writes to one SQLite database, so the chart runs exactly one pod with `Recreate` updates and one `ReadWriteOnce` volume at `/data`.

## Install

Create the secret key once and keep it safe. Losing it makes stored credentials unreadable.

```sh
openssl rand -hex 32
```

Install with an inline key (for tests only) or reference an existing Secret:

```sh
helm install gofer oci://ghcr.io/cristianadrielbraun/charts/gofer \
  --set config.baseUrl=https://mail.example.com \
  --set secret.secretKey=<64 hex chars>
```

Recommended: create a Secret with `GOFER_SECRET_KEY` (plus OAuth/VAPID variables if used) and set `secret.existingSecret`.

```sh
kubectl create secret generic gofer-secrets \
  --from-literal=GOFER_SECRET_KEY=<64 hex chars>

helm install gofer oci://ghcr.io/cristianadrielbraun/charts/gofer \
  --set config.baseUrl=https://mail.example.com \
  --set secret.existingSecret=gofer-secrets
```

## Common values

| Value | Default | Purpose |
| --- | --- | --- |
| `config.baseUrl` | `""` (required) | Public https URL. |
| `config.authMode` | `personal` | `open`, `personal` or `managed`. |
| `config.trustedProxyCidrs` | `""` | Ingress/proxy CIDRs, so client IPs are honored. |
| `config.allowedCidrs` | `""` | Optional client network allowlist. |
| `config.googleFonts` | `false` | Let people pick Google Fonts under Appearance, served through Gofer. |
| `config.extraEnv` | `{}` | Extra non-secret `GOFER_*` variables. |
| `secret.existingSecret` | `""` | Secret containing all secret variables. |
| `secret.env` | `{}` | Extra secret variables (OAuth, VAPID), when not using `existingSecret`. |
| `persistence.size` | `1Gi` | Data volume size. |
| `persistence.existingClaim` | `""` | Reuse an existing PVC. |
| `ingress.enabled` | `false` | Expose through an Ingress. |
| `httpRoute.enabled` | `false` | Expose through a Gateway API HTTPRoute. |
| `probes.type` | `http` | `http` uses `/healthz` and `/readyz`. `tcp` only checks the socket. |

## Notes

- `config.baseUrl` must match the public URL. Passkeys, secure cookies and OAuth callbacks depend on it.
- Behind an ingress, set `config.trustedProxyCidrs` to the ingress pod CIDR.
- For long-lived `/api/events` streams, raise the proxy read timeout (the chart's `ci/ingress-values.yaml` shows nginx annotations).
- Use RWO block storage. SQLite WAL is not safe on NFS or SMB.
- The pod runs as UID/GID 1000 with a read-only root filesystem; `/tmp` is an emptyDir.

## Test

```sh
helm lint charts/gofer --strict --values charts/gofer/ci/default-values.yaml
helm template gofer charts/gofer --values charts/gofer/ci/ingress-values.yaml
```
