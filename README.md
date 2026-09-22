# hyper-ops

Automatically registers HyperShift hosted clusters (and the management cluster) in Argo CD.

hyper-ops watches `HostedCluster` objects (`hypershift.openshift.io`) on the management cluster and creates/maintains `argocd.argoproj.io/secret-type: cluster` Secrets so Argo CD (OpenShift GitOps) can deploy to them. It provisions a dedicated service account on each cluster and cleans up the Argo CD secret when the `HostedCluster` is deleted.

## How it works

For each labeled `HostedCluster`:

1. Reads the hosted cluster API server address and admin kubeconfig from the `<cluster-name>-admin-kubeconfig` Secret in the `HostedCluster` namespace.
2. Creates on **both** the management cluster (in-cluster, `https://kubernetes.default.svc`, secret name `in-cluster-local`) and the hosted cluster:
   - `ServiceAccount` `kube-system/hyper-ops-admin`
   - `ClusterRoleBinding` `hyper-ops-admin` → `cluster-admin`
   - `Secret` `kube-system/hyper-ops-admin-token` (service-account token)
3. Writes an Argo CD cluster secret into the GitOps namespace (default `openshift-gitops`):
   - `Opaque` Secret named after the cluster, labeled `argocd.argoproj.io/secret-type: cluster`
   - `data.name`, `data.server`, `data.config` (JSON with `bearerToken` and `tlsClientConfig.caData`)
   - `hyper-ops.cloudmonkey.org/*` labels from the `HostedCluster` are propagated, plus `hyper-ops.cloudmonkey.org/type: hosted|local`
4. On `HostedCluster` deletion, deletes the corresponding Argo CD cluster secret.

Only `HostedCluster`s carrying the `hyper-ops.cloudmonkey.org/enabled` label are reconciled.

## Prerequisites

- OpenShift with the HyperShift operator running (management cluster).
- OpenShift GitOps / Argo CD installed (provides the namespace the secrets are written to, default `openshift-gitops`).
- Each hosted cluster must have its `<cluster-name>-admin-kubeconfig` Secret present in the `HostedCluster` namespace (created by HyperShift).
- Cluster-admin (or equivalent) permissions to install the operator and to create `ServiceAccounts` / `ClusterRoleBindings`.

## Install

### Option A: OLM (operator bundle / catalog)

Images:

- Operator: `quay.io/cldmnky/hyper-ops:v0.0.7`
- Bundle: `quay.io/cldmnky/hyper-ops-bundle:v0.0.7` (channel `alpha`)
- Catalog: `quay.io/cldmnky/hyper-ops-catalog:v1.0.1`

Install mode: `AllNamespaces`. Minimum Kubernetes: 1.25.

```bash
# Example: subscribe via an existing catalog source, or create one:
kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: hyper-ops
  namespace: openshift-marketplace
spec:
  sourceType: grpc
  image: quay.io/cldmnky/hyper-ops-catalog:v1.0.1
EOF
```

Then create a `Subscription` for package `hyper-ops`, channel `alpha`, in the UI or via YAML.

### Option B: plain manifests (kustomize)

```bash
make deploy IMG=quay.io/cldmnky/hyper-ops:v0.0.7
```

This installs CRDs into `hyper-ops-system` and deploys the controller. To remove:

```bash
make undeploy   # remove operator
make uninstall  # remove CRDs
```

## Usage

### 1. Enroll a hosted cluster

```bash
kubectl label hostedcluster <cluster-name> -n <hostedcluster-namespace> \
  hyper-ops.cloudmonkey.org/enabled=true
```

The operator then creates the `hyper-ops-admin` service account on that hosted cluster and writes a Secret named `<cluster-name>` into `openshift-gitops`.

### 2. Use a non-default Argo CD namespace

```bash
kubectl label hostedcluster <cluster-name> -n <hostedcluster-namespace> \
  hyper-ops.cloudmonkey.org/gitops-namespace=my-gitops-ns
```

Without this label, secrets go to `openshift-gitops`.

### 3. Opt out

```bash
kubectl label hostedcluster <cluster-name> -n <hostedcluster-namespace> \
  hyper-ops.cloudmonkey.org/enabled=false --overwrite
```

With `enabled=false` the cluster is skipped. Removing the label entirely also stops reconciliation (create/update events are filtered on the presence of the label). Deleting the `HostedCluster` removes its Argo CD secret.

## Verify

```bash
# Argo CD cluster secrets
kubectl get secrets -n openshift-gitops -l argocd.argoproj.io/secret-type=cluster

# Inspect one
kubectl get secret <cluster-name> -n openshift-gitops -o yaml

# Service accounts created by the operator
kubectl get sa hyper-ops-admin -n kube-system  # management cluster and hosted clusters
```

## Configuration reference

| Label | On | Effect |
|---|---|---|
| `hyper-ops.cloudmonkey.org/enabled=true` | `HostedCluster` | Enroll the cluster. Required; clusters without it are ignored. |
| `hyper-ops.cloudmonkey.org/enabled=false` | `HostedCluster` | Skip the cluster. |
| `hyper-ops.cloudmonkey.org/gitops-namespace=<ns>` | `HostedCluster` | Write the Argo CD secret to `<ns>` instead of `openshift-gitops`. |
| `hyper-ops.cloudmonkey.org/type=hosted\|local` | Argo CD `Secret` | Set by the operator; `local` = management cluster (`in-cluster-local`), `hosted` = hosted cluster. |

All other `hyper-ops.cloudmonkey.org/*` labels on the `HostedCluster` are copied onto its Argo CD secret.

## Notes and limitations

- The bundled `Config` CRD (`configs.hyper-ops.cloudmonkey.org`) is currently a placeholder with no controller behavior; enrollment is driven entirely by `HostedCluster` labels.
- The operator grants the provisioned `hyper-ops-admin` service account `cluster-admin` on each target cluster. Scope this to your security requirements.
- Requires the HyperShift `<cluster-name>-admin-kubeconfig` secret; clusters without it are requeued, not registered.
- Only `AllNamespaces` install mode is supported.

## License

Apache-2.0. See the repository for the full text.
