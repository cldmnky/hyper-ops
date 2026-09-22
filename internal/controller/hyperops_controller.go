/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kubernetes-client/go-base/config/api"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// hyperOpsLabelPrefix is the prefix of every label and finalizer owned by this
	// controller.
	hyperOpsLabelPrefix = "hyper-ops.cloudmonkey.org"

	// hyperOpsEnabledLabel is set by users to "false" to opt a HostedCluster out.
	// The label is absent by default, which means hyper-ops is enabled.
	hyperOpsEnabledLabel = hyperOpsLabelPrefix + "/enabled"
	// hyperOpsGitopsNamespaceLabel holds the namespace the ArgoCD cluster secrets
	// are written to. It defaults to defaultGitOpsNamespace when not set.
	hyperOpsGitopsNamespaceLabel = hyperOpsLabelPrefix + "/gitops-namespace"
	// hyperOpsTypeLabel marks the cluster entries created by this controller.
	hyperOpsTypeLabel = hyperOpsLabelPrefix + "/type"

	hyperOpsTypeLocal  = "local"
	hyperOpsTypeHosted = "hosted"

	// hyperOpsFinalizer is added to HostedClusters so the ArgoCD cluster secrets
	// can be removed before the HostedCluster disappears.
	hyperOpsFinalizer = hyperOpsLabelPrefix + "/finalizer"

	// hyperOpsManagedLabel marks the objects this controller creates in the
	// target cluster.
	hyperOpsManagedLabel = hyperOpsLabelPrefix + "/managed-by"
	hyperOpsManagedValue = "hyper-ops"

	argoCDSecretTypeLabel   = "argocd.argoproj.io/secret-type"
	argoCDSecretTypeCluster = "cluster"

	// Secret data keys.
	kubeconfigDataKey = "kubeconfig"
	caCertDataKey     = "ca.crt"
	tokenDataKey      = "token"

	// defaultGitOpsNamespace is used when a HostedCluster does not select a
	// namespace with the hyperOpsGitopsNamespaceLabel label.
	defaultGitOpsNamespace = "openshift-gitops"

	// in-cluster (management cluster) ArgoCD cluster entry.
	localClusterName   = "in-cluster-local"
	localClusterServer = "https://kubernetes.default.svc"

	// Credentials created in the target cluster for ArgoCD.
	hostedClusterServiceAccountName      = "hyper-ops-admin"
	hostedClusterServiceAccountNamespace = "kube-system"
	serviceAccountTokenSecretName        = hostedClusterServiceAccountName + "-token"
	clusterAdminRoleName                 = "cluster-admin"

	// adminKubeconfigSecretSuffix is appended to the HostedCluster name to find
	// the admin kubeconfig secret HyperShift creates.
	adminKubeconfigSecretSuffix = "-admin-kubeconfig"

	// requeueInterval is used when a dependent resource (for example the
	// HyperShift generated admin kubeconfig secret or the ServiceAccount token)
	// does not exist yet. A fixed requeue interval is used instead of the
	// exponential backoff that follows a returned error.
	requeueInterval = 30 * time.Second
)

// errResourceNotReady signals that a dependent resource does not exist yet. It is
// handled with a fixed requeue interval instead of an error.
var errResourceNotReady = errors.New("resource not ready")

// Cluster is the ArgoCD cluster entry for a Kubernetes cluster.
type Cluster struct {
	// Name is the name the cluster is registered with in ArgoCD.
	Name string `json:"name"`
	// Server is the API server URL of the cluster.
	Server string `json:"server"`
	// Config holds the credentials ArgoCD uses to talk to the cluster.
	Config ClusterConfig `json:"clusterConfig"`
}

// ClusterConfig holds the credentials for a cluster.
type ClusterConfig struct {
	BearerToken     string          `json:"bearerToken"`
	TLSClientConfig TLSClientConfig `json:"tlsClientConfig"`
}

// TLSClientConfig holds the TLS settings for a cluster.
type TLSClientConfig struct {
	// CAData is the standard base64 encoded CA certificate of the cluster.
	CAData string `json:"caData"`
}

// HyperOpsReconciler reconciles a HostedCluster object and keeps the ArgoCD
// cluster secrets of the management cluster and of the HostedCluster in sync.
type HyperOpsReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=hostedclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile ensures the ArgoCD cluster secrets for the management cluster and for
// the HostedCluster exist and are up to date.
//
// Deletion is handled with a finalizer: while the HostedCluster is terminating the
// ArgoCD cluster secrets it owns are removed and the finalizer is dropped
// afterwards. Reconciles that depend on a resource that does not exist yet (the
// admin kubeconfig secret or the ServiceAccount token) are requeued with a fixed
// interval instead of being reported as an error.
func (r *HyperOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	hc := &hypershiftv1beta1.HostedCluster{}
	if err := r.Get(ctx, req.NamespacedName, hc); err != nil {
		if apierrors.IsNotFound(err) {
			// The finalizer based cleanup runs while the object still exists, so
			// there is nothing left to do once it is gone.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching HostedCluster %s: %w", req.NamespacedName, err)
	}

	// The GitOps namespace is derived from the HostedCluster on every reconcile
	// and passed around explicitly. It is never stored in package state, which
	// would leak between HostedClusters and between concurrent reconciles.
	gitOpsNamespace := gitOpsNamespaceFor(hc)

	if !hc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, hc, gitOpsNamespace)
	}

	// Skip all work, including the finalizer and any provisioning side effect,
	// when the HostedCluster explicitly opts out.
	if enabled, ok := hc.GetLabels()[hyperOpsEnabledLabel]; ok && enabled == "false" {
		logger.Info("hyper-ops is disabled for HostedCluster, nothing to do", "hostedcluster", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	if err := r.ensureFinalizer(ctx, hc); err != nil {
		return ctrl.Result{}, err
	}

	// The local cluster entry always points at the management cluster itself.
	localCluster, err := r.setupClusterConfig(ctx, r.Client, localClusterServer, localClusterName)
	if err != nil {
		if result, ok := requeueIfNotReady(err); ok {
			logger.Info("in-cluster credentials are not ready yet, requeuing",
				"hostedcluster", req.NamespacedName, "requeueAfter", requeueInterval)
			return result, nil
		}
		return ctrl.Result{}, fmt.Errorf("setting up in-cluster config for HostedCluster %s: %w", req.NamespacedName, err)
	}

	localClusterLabels := map[string]string{hyperOpsTypeLabel: hyperOpsTypeLocal}
	if err := r.createArgoCDClusterSecret(ctx, gitOpsNamespace, hc, localClusterLabels, localCluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("creating in-cluster argocd cluster secret for HostedCluster %s: %w", req.NamespacedName, err)
	}

	// The admin kubeconfig is created by HyperShift once the control plane is
	// reachable. If it does not exist yet the reconcile is requeued with a fixed
	// interval; a watch on the secret also enqueues the HostedCluster as soon as
	// the secret appears.
	kubeConfigSecretKey := client.ObjectKey{Namespace: req.Namespace, Name: adminKubeconfigSecretName(req.Name)}
	kubeConfigSecret := &corev1.Secret{}
	if err := r.Get(ctx, kubeConfigSecretKey, kubeConfigSecret); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("admin kubeconfig secret does not exist yet, requeuing",
				"hostedcluster", req.NamespacedName, "secret", kubeConfigSecretKey, "requeueAfter", requeueInterval)
			return ctrl.Result{RequeueAfter: requeueInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching admin kubeconfig secret %s for HostedCluster %s: %w",
			kubeConfigSecretKey, req.NamespacedName, err)
	}

	hostedClusterClient, err := GetClientForCluster(kubeConfigSecret.Data[kubeconfigDataKey])
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating client for admin kubeconfig secret %s: %w", kubeConfigSecretKey, err)
	}

	server, err := r.getServerFromKubeConfig(kubeConfigSecret)
	if err != nil {
		return ctrl.Result{}, err
	}

	hostedClusterConfig, err := r.setupClusterConfig(ctx, hostedClusterClient, server, hc.Name)
	if err != nil {
		if result, ok := requeueIfNotReady(err); ok {
			logger.Info("hosted cluster credentials are not ready yet, requeuing",
				"hostedcluster", req.NamespacedName, "requeueAfter", requeueInterval)
			return result, nil
		}
		return ctrl.Result{}, fmt.Errorf("setting up hosted cluster config for HostedCluster %s: %w", req.NamespacedName, err)
	}

	// Copy the hyper-ops labels only: hc.GetLabels() is the live map of the cached
	// object and must never be mutated, otherwise the changes would be persisted
	// with the next update of the HostedCluster.
	hostedClusterLabels := map[string]string{}
	for k, v := range hc.GetLabels() {
		if strings.HasPrefix(k, hyperOpsLabelPrefix) {
			hostedClusterLabels[k] = v
		}
	}
	hostedClusterLabels[hyperOpsTypeLabel] = hyperOpsTypeHosted

	if err := r.createArgoCDClusterSecret(ctx, gitOpsNamespace, hc, hostedClusterLabels, hostedClusterConfig); err != nil {
		return ctrl.Result{}, fmt.Errorf("creating argocd cluster secret for HostedCluster %s: %w", req.NamespacedName, err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HyperOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hypershiftv1beta1.HostedCluster{},
			builder.WithPredicates(hyperOpsHostedClusterPredicate())).
		// The admin kubeconfig secret is not owned by the HostedCluster, so a
		// mapping function is used to enqueue the HostedCluster whenever the
		// secret appears (or changes).
		Watches(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapAdminKubeconfigSecret),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return isAdminKubeconfigSecretName(obj.GetName())
			}))).
		Complete(r)
}

// hyperOpsHostedClusterPredicate filters HostedCluster events: only HostedClusters
// that select hyper-ops with the enabled label are reconciled.
func hyperOpsHostedClusterPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return hasHyperOpsEnabledLabel(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return hasHyperOpsEnabledLabel(e.ObjectNew)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return hasHyperOpsEnabledLabel(e.Object)
		},
	}
}

func hasHyperOpsEnabledLabel(obj client.Object) bool {
	if obj == nil {
		return false
	}
	_, ok := obj.GetLabels()[hyperOpsEnabledLabel]
	return ok
}

// mapAdminKubeconfigSecret enqueues the HostedCluster an admin kubeconfig secret
// belongs to (the secret is named <hostedcluster>-admin-kubeconfig).
func (r *HyperOpsReconciler) mapAdminKubeconfigSecret(_ context.Context, obj client.Object) []reconcile.Request {
	name, ok := strings.CutSuffix(obj.GetName(), adminKubeconfigSecretSuffix)
	if !ok || name == "" {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}},
	}
}

// reconcileDelete removes the ArgoCD cluster secrets of a terminating
// HostedCluster and drops the finalizer once the cleanup succeeded. It returns an
// error (and therefore keeps the finalizer) when the cleanup failed.
func (r *HyperOpsReconciler) reconcileDelete(
	ctx context.Context, hc *hypershiftv1beta1.HostedCluster, gitOpsNamespace string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := client.ObjectKeyFromObject(hc)

	if !controllerutil.ContainsFinalizer(hc, hyperOpsFinalizer) {
		// Nothing was created for this HostedCluster.
		return ctrl.Result{}, nil
	}

	logger.Info("cleaning up hyper-ops resources for terminating HostedCluster",
		"hostedcluster", key, "gitopsNamespace", gitOpsNamespace)

	// The ArgoCD cluster secret of the HostedCluster is named after it; only
	// secrets that carry hyper-ops labels are removed.
	hostedSecretKey := client.ObjectKey{Namespace: gitOpsNamespace, Name: hc.Name}
	if err := r.deleteManagedSecret(ctx, hostedSecretKey); err != nil {
		return ctrl.Result{}, fmt.Errorf("deleting argocd cluster secret %s for HostedCluster %s: %w", hostedSecretKey, key, err)
	}

	// The in-cluster secret is shared by all HostedClusters, so it is only removed
	// when this HostedCluster actually controls it.
	localSecretKey := client.ObjectKey{Namespace: gitOpsNamespace, Name: localClusterName}
	if err := r.deleteSecretIfControlledBy(ctx, localSecretKey, hc); err != nil {
		return ctrl.Result{}, fmt.Errorf("deleting in-cluster argocd cluster secret %s for HostedCluster %s: %w", localSecretKey, key, err)
	}

	// Remove the finalizer on a deep copy so the cached object handed to Reconcile
	// is never mutated in place.
	patch := client.MergeFrom(hc.DeepCopy())
	controllerutil.RemoveFinalizer(hc, hyperOpsFinalizer)
	if err := r.Patch(ctx, hc, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer from HostedCluster %s: %w", key, err)
	}
	logger.Info("removed hyper-ops finalizer", "hostedcluster", key)
	return ctrl.Result{}, nil
}

// ensureFinalizer adds the hyper-ops finalizer to the HostedCluster if needed.
func (r *HyperOpsReconciler) ensureFinalizer(ctx context.Context, hc *hypershiftv1beta1.HostedCluster) error {
	if controllerutil.ContainsFinalizer(hc, hyperOpsFinalizer) {
		return nil
	}
	patch := client.MergeFrom(hc.DeepCopy())
	controllerutil.AddFinalizer(hc, hyperOpsFinalizer)
	if err := r.Patch(ctx, hc, patch); err != nil {
		return fmt.Errorf("adding finalizer to HostedCluster %s: %w", client.ObjectKeyFromObject(hc), err)
	}
	return nil
}

// deleteManagedSecret deletes a Secret when it exists and carries hyper-ops
// labels. A missing Secret is ignored and Secrets that were not created by this
// controller are left alone.
func (r *HyperOpsReconciler) deleteManagedSecret(ctx context.Context, key client.ObjectKey) error {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, key, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !isHyperOpsManaged(secret) {
		log.FromContext(ctx).V(1).Info("leaving secret that is not managed by hyper-ops in place", "secret", key)
		return nil
	}
	if err := r.Delete(ctx, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

// isHyperOpsManaged reports whether the object carries at least one hyper-ops
// label, which means it was created by this controller.
func isHyperOpsManaged(obj client.Object) bool {
	for k := range obj.GetLabels() {
		if strings.HasPrefix(k, hyperOpsLabelPrefix) {
			return true
		}
	}
	return false
}

// deleteSecretIfControlledBy deletes a Secret only when it carries a controller
// owner reference for owner. Shared secrets therefore survive the deletion of a
// single HostedCluster.
func (r *HyperOpsReconciler) deleteSecretIfControlledBy(ctx context.Context, key client.ObjectKey, owner client.Object) error {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, key, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(secret, owner) {
		return nil
	}
	if err := r.Delete(ctx, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

// createArgoCDClusterSecret creates or updates the ArgoCD cluster secret for the
// given cluster in the GitOps namespace.
func (r *HyperOpsReconciler) createArgoCDClusterSecret(
	ctx context.Context, gitOpsNamespace string, owner *hypershiftv1beta1.HostedCluster, labels map[string]string, cluster *Cluster,
) error {
	logger := log.FromContext(ctx)
	key := client.ObjectKey{Namespace: gitOpsNamespace, Name: cluster.Name}

	jsonConfig, err := json.Marshal(cluster.Config)
	if err != nil {
		return fmt.Errorf("marshalling config of cluster %s: %w", key, err)
	}

	// Copy the labels: the caller's map (which may be derived from the
	// HostedCluster) is never mutated.
	secretLabels := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		secretLabels[k] = v
	}
	secretLabels[argoCDSecretTypeLabel] = argoCDSecretTypeCluster

	argocdCluster := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: gitOpsNamespace,
			Labels:    secretLabels,
		},
	}
	op, err := CreateOrUpdateWithRetries(ctx, r.Client, argocdCluster, func() error {
		argocdCluster.Labels = secretLabels
		argocdCluster.Data = map[string][]byte{
			"name":   []byte(cluster.Name),
			"server": []byte(cluster.Server),
			"config": jsonConfig,
		}
		argocdCluster.Type = corev1.SecretTypeOpaque
		return setControllerReferenceIfPossible(owner, argocdCluster, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensuring argocd cluster secret %s: %w", key, err)
	}
	logger.V(1).Info("argocd cluster secret ensured", "secret", key, "op", op)
	return nil
}

// setControllerReferenceIfPossible sets a controller reference on the secret when
// it lives in the same namespace as the owner. Kubernetes only resolves owner
// references within a single namespace, so the ArgoCD secrets in the GitOps
// namespace are not owned by a HostedCluster living somewhere else.
func setControllerReferenceIfPossible(owner *hypershiftv1beta1.HostedCluster, secret *corev1.Secret, scheme *runtime.Scheme) error {
	if owner == nil || scheme == nil || secret.Namespace != owner.Namespace {
		return nil
	}
	return controllerutil.SetControllerReference(owner, secret, scheme)
}

// getServerFromKubeConfig returns the API server URL of the kubeconfig stored in
// the admin kubeconfig secret.
func (r *HyperOpsReconciler) getServerFromKubeConfig(kubeConfigSecret *corev1.Secret) (string, error) {
	key := client.ObjectKeyFromObject(kubeConfigSecret)
	raw, ok := kubeConfigSecret.Data[kubeconfigDataKey]
	if !ok || len(raw) == 0 {
		return "", fmt.Errorf("secret %s does not contain a %q entry", key, kubeconfigDataKey)
	}

	kubeconfig := api.Config{}
	if err := yaml.Unmarshal(raw, &kubeconfig); err != nil {
		return "", fmt.Errorf("parsing %q of secret %s: %w", kubeconfigDataKey, key, err)
	}
	// The kubeconfig generated by HyperShift contains a single cluster entry. The
	// length must be checked: indexing an empty slice panics and would take down
	// the worker.
	if len(kubeconfig.Clusters) == 0 {
		return "", fmt.Errorf("%q of secret %s does not contain any cluster", kubeconfigDataKey, key)
	}
	for _, namedCluster := range kubeconfig.Clusters {
		if namedCluster.Cluster.Server != "" {
			return namedCluster.Cluster.Server, nil
		}
	}
	return "", fmt.Errorf("%q of secret %s does not contain a cluster with a server", kubeconfigDataKey, key)
}

// gitOpsNamespaceFor returns the namespace the ArgoCD cluster secrets are written
// to: the hyper-ops gitops-namespace label when it is set, otherwise the
// defaultGitOpsNamespace. The namespace is resolved for every reconcile, so
// concurrent reconciles of different HostedClusters cannot leak a namespace into
// each other.
func gitOpsNamespaceFor(hc *hypershiftv1beta1.HostedCluster) string {
	if namespace := strings.TrimSpace(hc.GetLabels()[hyperOpsGitopsNamespaceLabel]); namespace != "" {
		return namespace
	}
	return defaultGitOpsNamespace
}

// adminKubeconfigSecretName returns the name of the admin kubeconfig secret
// HyperShift creates for a HostedCluster.
func adminKubeconfigSecretName(hostedClusterName string) string {
	return hostedClusterName + adminKubeconfigSecretSuffix
}

// isAdminKubeconfigSecretName reports whether the given secret name is an admin
// kubeconfig secret.
func isAdminKubeconfigSecretName(name string) bool {
	hostedClusterName, ok := strings.CutSuffix(name, adminKubeconfigSecretSuffix)
	return ok && hostedClusterName != ""
}

// requeueIfNotReady converts a not ready dependent resource into a requeue that
// uses a fixed interval.
func requeueIfNotReady(err error) (ctrl.Result, bool) {
	if errors.Is(err, errResourceNotReady) {
		return ctrl.Result{RequeueAfter: requeueInterval}, true
	}
	return ctrl.Result{}, false
}

// setupClusterConfig ensures the ServiceAccount, its ClusterRoleBinding and the
// ServiceAccount token secret exist in the target cluster and returns the
// credentials ArgoCD uses for it.
func (r *HyperOpsReconciler) setupClusterConfig(
	ctx context.Context, clnt client.Client, server string, name string,
) (*Cluster, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("setting up cluster config", "name", name, "server", server)

	managedLabels := map[string]string{hyperOpsManagedLabel: hyperOpsManagedValue}

	saKey := client.ObjectKey{Namespace: hostedClusterServiceAccountNamespace, Name: hostedClusterServiceAccountName}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saKey.Name, Namespace: saKey.Namespace}}
	op, err := CreateOrUpdateWithRetries(ctx, clnt, sa, func() error {
		sa.Labels = mergeStringMaps(sa.Labels, managedLabels)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ensuring ServiceAccount %s: %w", saKey, err)
	}
	logger.V(1).Info("service account ensured", "serviceaccount", saKey, "op", op)

	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: hostedClusterServiceAccountName}}
	op, err = CreateOrUpdateWithRetries(ctx, clnt, crb, func() error {
		crb.Labels = mergeStringMaps(crb.Labels, managedLabels)
		crb.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     clusterAdminRoleName,
		}
		crb.Subjects = []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      hostedClusterServiceAccountName,
				Namespace: hostedClusterServiceAccountNamespace,
			},
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ensuring ClusterRoleBinding %s: %w", hostedClusterServiceAccountName, err)
	}
	logger.V(1).Info("cluster role binding ensured", "clusterrolebinding", hostedClusterServiceAccountName, "op", op)

	tokenKey := client.ObjectKey{Namespace: hostedClusterServiceAccountNamespace, Name: serviceAccountTokenSecretName}
	saTokenSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tokenKey.Name, Namespace: tokenKey.Namespace}}
	op, err = CreateOrUpdateWithRetries(ctx, clnt, saTokenSecret, func() error {
		saTokenSecret.Labels = mergeStringMaps(saTokenSecret.Labels, managedLabels)
		if saTokenSecret.Annotations == nil {
			saTokenSecret.Annotations = map[string]string{}
		}
		saTokenSecret.Annotations[corev1.ServiceAccountNameKey] = hostedClusterServiceAccountName
		saTokenSecret.Type = corev1.SecretTypeServiceAccountToken
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ensuring ServiceAccount token secret %s: %w", tokenKey, err)
	}
	logger.V(1).Info("service account token secret ensured", "secret", tokenKey, "op", op)

	// The token and the CA bundle are filled in by the ServiceAccount token
	// controller, which can take a moment.
	if err := clnt.Get(ctx, tokenKey, saTokenSecret); err != nil {
		return nil, fmt.Errorf("fetching ServiceAccount token secret %s: %w", tokenKey, err)
	}
	token := saTokenSecret.Data[tokenDataKey]
	if len(token) == 0 {
		return nil, fmt.Errorf("token not found in secret %s: %w", tokenKey, errResourceNotReady)
	}
	caCrt := saTokenSecret.Data[caCertDataKey]
	if len(caCrt) == 0 {
		return nil, fmt.Errorf("ca.crt not found in secret %s: %w", tokenKey, errResourceNotReady)
	}

	return &Cluster{
		Name:   name,
		Server: server,
		Config: ClusterConfig{
			BearerToken: string(token),
			TLSClientConfig: TLSClientConfig{
				CAData: base64.StdEncoding.EncodeToString(caCrt),
			},
		},
	}, nil
}
