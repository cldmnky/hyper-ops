# hyper-ops
A controller for setting up ArgoCD clusters from Hosted control planes in HyperShift


# Installation

For now, clone and run `make deploy`, make sure your context is setup towards the cluster running your hypershift operator.

You should also have the OpenShift gitops operator installed.


# Usage

The controller will create a secret (as an ArgoCD cluster secret) in the `"openshift-gitops"` namespace for every `hostedcluster` resource that have the label `hyper-ops.cloudmonkey.org/enabled=true` set.

The cluster secret will also have any labels add from the `hostedcluster`instance.

The cluster may easily be used in ArgoCD `ApplicationSets` for simple multicluster gitops. 