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

package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kubernetes-client/go-base/config/api"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/v1beta1"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	hyperOpsLabel = "hyper-ops.cloudmonkey.org"

	argoCDSecretTypeLabel   = "argocd.argoproj.io/secret-type"
	argoCDSecretTypeCluster = "cluster"

	hostedClusterServiceAccountName      = "hyper-ops-admin"
	hostedClusterServiceAccountNamespace = "kube-system"
)

var (
	hyperOpsEnabledLabel         = fmt.Sprintf("%s/enabled", hyperOpsLabel)
	hyperOpsGitopsNamespaceLabel = fmt.Sprintf("%s/gitops-namespace", hyperOpsLabel)
	gitOpsNamespace              = "openshift-gitops"
)

type Cluster struct {
	Name          string        `json:"name"`
	Server        string        `json:"server"`
	Config        ClusterConfig `json:"clusterConfig"`
	HostedCluster *hypershiftv1beta1.HostedCluster
}

type ClusterConfig struct {
	BearerToken     string          `json:"bearerToken"`
	TLSClientConfig TLSClientConfig `json:"tlsClientConfig"`
}
type TLSClientConfig struct {
	CAData string `json:"caData"`
}

// ConfigReconciler reconciles a Config object
type HyperOpsReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=hostedclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
func (r *HyperOpsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	hc := &hypershiftv1beta1.HostedCluster{}
	if err := r.Get(ctx, req.NamespacedName, hc); err != nil {
		log.V(3).Error(err, "unable to fetch HostedCluster")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// TODO: Handle deletion
	if hc.DeletionTimestamp != nil {
		log.Info("HostedCluster is being deleted")
		// cleanup secret
		if err := r.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      req.Name,
				Namespace: gitOpsNamespace,
			},
		}); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
	}
	// check if the hostedcluster has defined the gitops namespace
	if _, ok := hc.GetLabels()[hyperOpsGitopsNamespaceLabel]; !ok {
		log.V(3).Info("HostedCluster does not have the gitops namespace label, using default namespace: openshift-gitops")
	} else {
		gitOpsNamespace = hc.GetLabels()[hyperOpsGitopsNamespaceLabel]
	}
	// create the service account for the local cluster
	localCluster, err := r.setupClusterConfig(ctx, r.Client, "https://kubernetes.default.svc", "in-cluster-local", nil)
	if err != nil {
		log.V(3).Error(err, "unable to create in-cluster config")
		return ctrl.Result{}, err
	}

	localClusterLabels := map[string]string{
		"hyper-ops.cloudmonkey.org/type": "local",
	}

	if err := r.createArgoCDClusterSecret(ctx, localClusterLabels, localCluster); err != nil {
		log.V(3).Error(err, "unable to create in-cluster argocd cluster secret")
		return ctrl.Result{}, err
	}

	// skip if the hosted cluster sets the label to false
	if enabled, ok := hc.GetLabels()[hyperOpsEnabledLabel]; ok && enabled == "false" {
		log.V(3).Info("HostedCluster have the hyper-ops enabled label set to false")
		return ctrl.Result{}, nil
	}
	// get the kubeconfig for the hosted cluster
	kubeConfigSecret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: fmt.Sprintf("%s-admin-kubeconfig", req.Name)}, kubeConfigSecret); err != nil {
		log.V(3).Error(err, "unable to fetch kubeconfig secret")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	hostedClusterClient, err := GetClientForCluster(kubeConfigSecret.Data["kubeconfig"])
	if err != nil {
		log.V(3).Error(err, "unable to create hosted cluster client")
		return ctrl.Result{}, err
	}

	server, err := r.getServerFromKubeConfig(kubeConfigSecret)
	if err != nil {
		log.V(3).Error(err, "unable to get server from kubeconfig")
		return ctrl.Result{}, err
	}

	hostedClusterConfig, err := r.setupClusterConfig(ctx, hostedClusterClient, server, hc.Name, hc)
	if err != nil {
		log.V(3).Error(err, "unable to create hosted cluster config")
		return ctrl.Result{}, err
	}

	hostedClusterLabels := hc.GetLabels()
	// only keep the labels that are related to hyper-ops
	for k := range hostedClusterLabels {
		if !strings.HasPrefix(k, hyperOpsLabel) {
			delete(hostedClusterLabels, k)
		}
	}
	hostedClusterLabels["hyper-ops.cloudmonkey.org/type"] = "hosted"

	if err := r.createArgoCDClusterSecret(ctx, hostedClusterLabels, hostedClusterConfig); err != nil {
		log.V(3).Error(err, "unable to create argocd cluster secret")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HyperOpsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hypershiftv1beta1.HostedCluster{}).
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				if _, ok := e.ObjectNew.GetLabels()[hyperOpsEnabledLabel]; !ok {
					return false
				}
				mgr.GetLogger().Info("watching", e.ObjectNew.GetObjectKind().GroupVersionKind().String(), e.ObjectNew.GetName())

				return true
			},
			CreateFunc: func(e event.CreateEvent) bool {
				if _, ok := e.Object.GetLabels()[hyperOpsEnabledLabel]; !ok {
					return false
				}
				mgr.GetLogger().Info("watching", e.Object.GetObjectKind().GroupVersionKind().String(), e.Object.GetName())
				return true
			},
		}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

func (r *HyperOpsReconciler) createArgoCDClusterSecret(ctx context.Context, labels map[string]string, cluster *Cluster) error {
	log := log.FromContext(ctx)
	// create the secret for the local cluster
	argocdClusterLabels := labels
	argocdClusterLabels[argoCDSecretTypeLabel] = argoCDSecretTypeCluster

	jsonConfig, err := json.Marshal(cluster.Config)
	if err != nil {
		return err
	}

	argocdCluster := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: gitOpsNamespace,
		},
	}
	op, err := CreateOrUpdateWithRetries(ctx, r.Client, argocdCluster, func() error {
		argocdCluster.Labels = argocdClusterLabels
		argocdCluster.Data = map[string][]byte{
			"name":   []byte(cluster.Name),
			"server": []byte(cluster.Server),
			"config": jsonConfig,
		}
		argocdCluster.Type = corev1.SecretTypeOpaque
		return nil
	})
	if err != nil {
		log.V(3).Error(err, "unable to ensure argo cluster secret")
		return err
	}
	log.V(3).Info("argocd cluster secret", "op", op)
	return nil
}

func (r *HyperOpsReconciler) getServerFromKubeConfig(kubeConfigSecret *corev1.Secret) (string, error) {
	kubeconfig := api.Config{}
	if err := yaml.Unmarshal(kubeConfigSecret.Data["kubeconfig"], &kubeconfig); err != nil {
		return "", err
	}
	return kubeconfig.Clusters[0].Cluster.Server, nil
}

func (r *HyperOpsReconciler) setupClusterConfig(ctx context.Context, clnt client.Client, server string, name string, hc *hypershiftv1beta1.HostedCluster) (*Cluster, error) {
	log := log.FromContext(ctx)
	log.Info("setting up cluster config", "name", name, "server", server)
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hostedClusterServiceAccountName,
			Namespace: hostedClusterServiceAccountNamespace,
		},
	}
	op, err := CreateOrUpdateWithRetries(ctx, clnt, sa, func() error {
		return nil
	})
	if err != nil {
		log.V(3).Error(err, "unable to ensure hosted cluster service account")
		return nil, err
	}
	log.V(3).Info("service account created", "op", op)
	// create a cluster role binding
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: hostedClusterServiceAccountName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      hostedClusterServiceAccountName,
				Namespace: hostedClusterServiceAccountNamespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}
	op, err = CreateOrUpdateWithRetries(ctx, clnt, crb, func() error {
		return nil
	})
	if err != nil {
		log.V(3).Error(err, "unable to ensure hosted cluster cluster role binding")
		return nil, err
	}
	log.V(3).Info("cluster role binding created", "op", op)

	// Create an sa token secret
	saTokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-token", hostedClusterServiceAccountName),
			Namespace: hostedClusterServiceAccountNamespace,
			Annotations: map[string]string{
				corev1.ServiceAccountNameKey: sa.Name,
			},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}
	op, err = CreateOrUpdateWithRetries(ctx, clnt, saTokenSecret, func() error {
		return nil
	})
	if err != nil {
		log.V(3).Error(err, "unable to ensure hosted cluster service account token")
		return nil, err
	}
	log.V(3).Info("service account token created", "op", op)

	// Get the token secret
	if err := clnt.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "hyper-ops-admin-token"}, saTokenSecret); err != nil {
		log.V(3).Error(err, "unable to get hosted cluster secret")
		return nil, err
	}
	if len(saTokenSecret.Data["token"]) == 0 {
		return nil, fmt.Errorf("token not found")
	}
	if len(saTokenSecret.Data["ca.crt"]) == 0 {
		return nil, fmt.Errorf("ca.crt not found")
	}
	// create the cluster config
	return &Cluster{
		Name:   name,
		Server: server,
		Config: ClusterConfig{
			BearerToken: string(saTokenSecret.Data["token"]),
			TLSClientConfig: TLSClientConfig{
				CAData: base64.URLEncoding.EncodeToString(saTokenSecret.Data["ca.crt"]),
			},
		},
		HostedCluster: hc,
	}, nil
}
