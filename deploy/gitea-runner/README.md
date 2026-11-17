# Gitea Actions runner (optional)

The pipeline in `.gitea/workflows/` runs only if an `act_runner` is registered. This directory is the
Kubernetes variant, in **job-pods mode**: each job runs in its own pod under the cluster's quotas, with no
privileged container and no Docker socket.

```sh
# 1. registration token (admin, expires in an hour)
curl -sS -H "Authorization: token $TOKEN" \
  "$GITEA/api/v1/admin/actions/runners/registration-token" | jq -r .token

# 2. secret + objects (namespace: gitea-runner)
kubectl create ns gitea-runner
kubectl -n gitea-runner create secret generic runner-token --from-literal=token=<the token above>
kubectl apply -f gitea-runner.yaml

# 3. confirm it registered and picked a label up
kubectl -n gitea-runner logs deploy/gitea-runner | grep -i "runner registered"
```

## What it can and cannot run

| supported | not supported |
|---|---|
| `run:` steps in any image (`kubernetes://golang:1.23`) | Docker actions and `docker://` steps (no daemon in job pods) |
| `actions/checkout@v4` (implemented by the runner itself) | anything needing a privileged container |

Job images therefore need `sh`, `tar`, `tail`, `env` (and `git`, `go` if the job builds).

## Where the pods go

Jobs run in the **runner's own namespace** with the runner's ServiceAccount, which is why the Role below
exists: `pods` create/get/list/delete, `pods/exec`, `pods/log`, `secrets` create/delete. It grants nothing
cluster-wide, and the runner itself never gets kubeconfig access to anything else.
