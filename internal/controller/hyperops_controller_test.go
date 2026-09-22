package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/api/util/ipnet"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	hyperOpsControllerBaseName = "test-hyperops"
	gitOpsNamespaceBaseName    = "openshift-gitops"

	// clusterNameLabel is set on the HostedCluster to check that hyper-ops labels
	// are copied to the ArgoCD cluster secret.
	clusterNameLabel = hyperOpsLabelPrefix + "/cluster-name"

	// enabledLabelValue is the value that opts a HostedCluster in.
	enabledLabelValue = "true"

	// Names of the helper secrets used by the getServerFromKubeConfig specs.
	testKubeconfigSecretName      = "kubeconfig"
	testKubeconfigSecretNamespace = "clusters"
)

// testCAData is chosen so that the standard and the URL base64 encodings differ.
var testCAData = []byte{0xfb, 0xef, 0xbe, 0xad, 0x01}

var _ = Describe("Hyper-Ops controller", func() {
	Context("hyper-ops controller test", func() {
		var (
			typeNamespaceName  types.NamespacedName
			namespace          *corev1.Namespace
			gitOpsNamespace    *corev1.Namespace
			hyperOpsReconciler *HyperOpsReconciler
			cluster            *hypershiftv1beta1.HostedCluster
		)

		ctx := context.Background()

		BeforeEach(func() {
			By("Creating the Namespaces to perform the tests")
			namespace = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("%s-%d", hyperOpsControllerBaseName, time.Now().UnixMilli()),
				},
			}
			Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
			typeNamespaceName = types.NamespacedName{Name: hyperOpsControllerBaseName, Namespace: namespace.Name}

			gitOpsNamespace = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("%s-%d", gitOpsNamespaceBaseName, time.Now().UnixMilli()),
				},
			}
			Expect(k8sClient.Create(ctx, gitOpsNamespace)).To(Succeed())

			By("Ensuring the default ArgoCD namespace exists")
			defaultNamespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: defaultGitOpsNamespace},
			}
			_, err := CreateOrUpdateWithRetries(ctx, k8sClient, defaultNamespace, func() error { return nil })
			Expect(err).NotTo(HaveOccurred())

			By("Creating a new HostedCluster without the hyper-ops labels")
			cluster = newHostedCluster(hyperOpsControllerBaseName, namespace.Name)
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			By("Creating a admin kubeconfig secret")
			kc, err := generateKubeConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
			adminKubeconfigSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      adminKubeconfigSecretName(hyperOpsControllerBaseName),
					Namespace: namespace.Name,
				},
				Data: map[string][]byte{
					kubeconfigDataKey: kc,
				},
			}
			Expect(k8sClient.Create(ctx, adminKubeconfigSecret)).To(Succeed())

			// Since we do not have controllers running we need to create the token
			// secret manually. The data is restored for every spec.
			By("Creating a token secret")
			tokenSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceAccountTokenSecretName,
					Namespace: hostedClusterServiceAccountNamespace,
				},
			}
			_, err = CreateOrUpdateWithRetries(ctx, k8sClient, tokenSecret, func() error {
				tokenSecret.Annotations = map[string]string{
					corev1.ServiceAccountNameKey: hostedClusterServiceAccountName,
				}
				tokenSecret.Type = corev1.SecretTypeServiceAccountToken
				tokenSecret.Data = map[string][]byte{
					tokenDataKey:  []byte("token"),
					caCertDataKey: testCAData,
				}
				return nil
			})
			Expect(err).NotTo(HaveOccurred())

			By("Creating a hyper ops reconciler")
			hyperOpsReconciler = &HyperOpsReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
		})

		AfterEach(func() {
			By("Removing the finalizer and deleting the HostedCluster")
			hc := &hypershiftv1beta1.HostedCluster{}
			err := k8sClient.Get(ctx, typeNamespaceName, hc)
			switch {
			case err == nil && controllerutil.ContainsFinalizer(hc, hyperOpsFinalizer):
				controllerutil.RemoveFinalizer(hc, hyperOpsFinalizer)
				Expect(k8sClient.Update(ctx, hc)).To(Succeed())
			case err == nil && hc.DeletionTimestamp.IsZero():
				Expect(k8sClient.Delete(ctx, hc)).To(Succeed())
			default:
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}

			By("Deleting the Namespaces to perform the tests")
			_ = k8sClient.Delete(ctx, namespace)
			_ = k8sClient.Delete(ctx, gitOpsNamespace)
		})

		Describe("HostedCluster event predicate", func() {
			It("only reconciles HostedClusters with the hyper-ops enabled label", func() {
				predicate := hyperOpsHostedClusterPredicate()
				unlabeled := &hypershiftv1beta1.HostedCluster{ObjectMeta: metav1.ObjectMeta{Name: "unlabeled"}}
				labeled := &hypershiftv1beta1.HostedCluster{ObjectMeta: metav1.ObjectMeta{
					Name:   "labeled",
					Labels: map[string]string{hyperOpsEnabledLabel: enabledLabelValue},
				}}

				Expect(predicate.Create(event.CreateEvent{Object: unlabeled})).To(BeFalse())
				Expect(predicate.Update(event.UpdateEvent{ObjectOld: unlabeled, ObjectNew: unlabeled})).To(BeFalse())
				Expect(predicate.Delete(event.DeleteEvent{Object: unlabeled})).To(BeFalse())

				Expect(predicate.Create(event.CreateEvent{Object: labeled})).To(BeTrue())
				Expect(predicate.Update(event.UpdateEvent{ObjectOld: unlabeled, ObjectNew: labeled})).To(BeTrue())
				Expect(predicate.Delete(event.DeleteEvent{Object: labeled})).To(BeTrue())
			})
		})

		Describe("Admin kubeconfig secret mapping", func() {
			It("maps an admin kubeconfig secret to its HostedCluster", func() {
				requests := hyperOpsReconciler.mapAdminKubeconfigSecret(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-cluster" + adminKubeconfigSecretSuffix,
						Namespace: testKubeconfigSecretNamespace,
					},
				})
				Expect(requests).To(Equal([]reconcile.Request{
					{NamespacedName: types.NamespacedName{Namespace: "clusters", Name: "my-cluster"}},
				}))

				for _, name := range []string{"unrelated-secret", adminKubeconfigSecretSuffix} {
					Expect(hyperOpsReconciler.mapAdminKubeconfigSecret(ctx, &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testKubeconfigSecretNamespace},
					})).To(BeEmpty())
				}
			})
		})

		Describe("getServerFromKubeConfig", func() {
			It("returns an error when the kubeconfig is missing", func() {
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testKubeconfigSecretName, Namespace: testKubeconfigSecretNamespace}}
				_, err := hyperOpsReconciler.getServerFromKubeConfig(secret)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(kubeconfigDataKey))
			})

			It("returns an error when the kubeconfig is malformed", func() {
				secret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: testKubeconfigSecretName, Namespace: testKubeconfigSecretNamespace},
					Data:       map[string][]byte{kubeconfigDataKey: []byte("server: [https://example.com")},
				}
				_, err := hyperOpsReconciler.getServerFromKubeConfig(secret)
				Expect(err).To(HaveOccurred())
			})

			It("returns an error when the kubeconfig has no clusters", func() {
				secret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: testKubeconfigSecretName, Namespace: testKubeconfigSecretNamespace},
					Data: map[string][]byte{kubeconfigDataKey: []byte(
						"apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\ncurrent-context: \"\"\n")},
				}
				_, err := hyperOpsReconciler.getServerFromKubeConfig(secret)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("does not contain any cluster"))
			})

			It("returns the server of the first cluster", func() {
				secret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: testKubeconfigSecretName, Namespace: testKubeconfigSecretNamespace},
					Data: map[string][]byte{kubeconfigDataKey: []byte(
						"apiVersion: v1\nkind: Config\nclusters:\n- name: cluster\n  cluster:\n    server: https://example.com:6443\n")},
				}
				server, err := hyperOpsReconciler.getServerFromKubeConfig(secret)
				Expect(err).NotTo(HaveOccurred())
				Expect(server).To(Equal("https://example.com:6443"))
			})
		})

		Describe("With the enabled label set to false", func() {
			It("does not create any resources", func() {
				By("Opting the HostedCluster out")
				Expect(k8sClient.Get(ctx, typeNamespaceName, cluster)).To(Succeed())
				cluster.Labels = map[string]string{
					hyperOpsEnabledLabel:         "false",
					hyperOpsGitopsNamespaceLabel: gitOpsNamespace.Name,
				}
				Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

				By("Reconciling the HostedCluster")
				_, err := hyperOpsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
				Expect(err).NotTo(HaveOccurred())

				By("Checking that no ArgoCD cluster secret was created and no finalizer was added")
				for _, name := range []string{localClusterName, hyperOpsControllerBaseName} {
					err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: gitOpsNamespace.Name}, &corev1.Secret{})
					Expect(apierrors.IsNotFound(err)).To(BeTrue(), "secret %s should not exist", name)
				}
				Expect(k8sClient.Get(ctx, typeNamespaceName, cluster)).To(Succeed())
				Expect(controllerutil.ContainsFinalizer(cluster, hyperOpsFinalizer)).To(BeFalse())
			})
		})

		Describe("When the admin kubeconfig secret does not exist", func() {
			It("requeues instead of reporting success", func() {
				By("Deleting the admin kubeconfig secret")
				Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
					Name:      adminKubeconfigSecretName(hyperOpsControllerBaseName),
					Namespace: namespace.Name,
				}})).To(Succeed())

				By("Reconciling the HostedCluster")
				result, err := hyperOpsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(requeueInterval))

				By("Checking that no ArgoCD cluster secret was created for the HostedCluster")
				err = k8sClient.Get(ctx, types.NamespacedName{Name: hyperOpsControllerBaseName, Namespace: gitOpsNamespace.Name}, &corev1.Secret{})
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			})
		})

		Describe("When the admin kubeconfig is malformed", func() {
			It("returns an error without panicking", func() {
				By("Writing a malformed kubeconfig")
				kubeConfigSecret := &corev1.Secret{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      adminKubeconfigSecretName(hyperOpsControllerBaseName),
					Namespace: namespace.Name,
				}, kubeConfigSecret)).To(Succeed())
				kubeConfigSecret.Data[kubeconfigDataKey] = []byte("this: [is: not: valid")
				Expect(k8sClient.Update(ctx, kubeConfigSecret)).To(Succeed())

				By("Reconciling the HostedCluster")
				_, err := hyperOpsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("admin kubeconfig secret"))
			})
		})

		Describe("When the ServiceAccount token is not ready", func() {
			It("requeues instead of reporting success", func() {
				By("Emptying the token secret")
				tokenSecret := &corev1.Secret{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      serviceAccountTokenSecretName,
					Namespace: hostedClusterServiceAccountNamespace,
				}, tokenSecret)).To(Succeed())
				tokenSecret.Data = nil
				Expect(k8sClient.Update(ctx, tokenSecret)).To(Succeed())

				By("Reconciling the HostedCluster")
				result, err := hyperOpsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(requeueInterval))
			})
		})

		Describe("With the enabled label", func() {
			It("reconciles the HostedCluster", func() {
				By("Labeling the HostedCluster")
				Expect(k8sClient.Get(ctx, typeNamespaceName, cluster)).To(Succeed())
				cluster.Labels = map[string]string{
					hyperOpsEnabledLabel:         enabledLabelValue,
					hyperOpsGitopsNamespaceLabel: gitOpsNamespace.Name,
				}
				Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

				By("Reconciling the HostedCluster")
				_, err := hyperOpsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
				Expect(err).NotTo(HaveOccurred())

				By("Checking the in-cluster ArgoCD secret")
				inClusterSecret := &corev1.Secret{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: localClusterName, Namespace: gitOpsNamespace.Name,
				}, inClusterSecret)).To(Succeed())
				Expect(inClusterSecret.Labels).To(HaveKeyWithValue(hyperOpsTypeLabel, hyperOpsTypeLocal))
				Expect(inClusterSecret.Labels).To(HaveKeyWithValue(argoCDSecretTypeLabel, argoCDSecretTypeCluster))

				By("Checking the HostedCluster ArgoCD secret")
				hostedSecret := &corev1.Secret{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: hyperOpsControllerBaseName, Namespace: gitOpsNamespace.Name,
				}, hostedSecret)).To(Succeed())
				Expect(hostedSecret.Labels).To(HaveKeyWithValue(hyperOpsTypeLabel, hyperOpsTypeHosted))
				Expect(hostedSecret.Labels).To(HaveKeyWithValue(hyperOpsEnabledLabel, enabledLabelValue))
				Expect(string(hostedSecret.Data["server"])).To(Equal(cfg.Host))
				Expect(hostedSecret.Data["name"]).To(Equal([]byte(hyperOpsControllerBaseName)))

				By("Checking the CA data is standard base64 encoded")
				clusterConfig := ClusterConfig{}
				Expect(json.Unmarshal(hostedSecret.Data["config"], &clusterConfig)).To(Succeed())
				Expect(clusterConfig.BearerToken).To(Equal("token"))
				Expect(clusterConfig.TLSClientConfig.CAData).To(Equal(base64.StdEncoding.EncodeToString(testCAData)))
				Expect(base64.URLEncoding.EncodeToString(testCAData)).NotTo(Equal(clusterConfig.TLSClientConfig.CAData))

				By("Checking the HostedCluster itself was not mutated")
				Expect(k8sClient.Get(ctx, typeNamespaceName, cluster)).To(Succeed())
				Expect(cluster.Labels).To(HaveKeyWithValue(hyperOpsEnabledLabel, enabledLabelValue))
				Expect(cluster.Labels).NotTo(HaveKey(hyperOpsTypeLabel))
				Expect(cluster.Labels).NotTo(HaveKey(argoCDSecretTypeLabel))
				Expect(controllerutil.ContainsFinalizer(cluster, hyperOpsFinalizer)).To(BeTrue())

				By("Updating the labels on the HostedCluster")
				Expect(k8sClient.Get(ctx, typeNamespaceName, cluster)).To(Succeed())
				cluster.Labels = map[string]string{
					hyperOpsEnabledLabel:         enabledLabelValue,
					hyperOpsGitopsNamespaceLabel: gitOpsNamespace.Name,
					clusterNameLabel:             "test",
				}
				Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

				By("Reconciling the HostedCluster again")
				_, err = hyperOpsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
				Expect(err).NotTo(HaveOccurred())

				By("Checking that the secret labels have been updated")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: hyperOpsControllerBaseName, Namespace: gitOpsNamespace.Name,
				}, hostedSecret)).To(Succeed())
				Expect(hostedSecret.Labels).To(HaveKeyWithValue(clusterNameLabel, "test"))
			})
		})

		Describe("With a manager watching the HostedClusters", func() {
			It("reconciles a labeled HostedCluster", func() {
				By("Labeling the HostedCluster")
				Expect(k8sClient.Get(ctx, typeNamespaceName, cluster)).To(Succeed())
				cluster.Labels = map[string]string{
					hyperOpsEnabledLabel:         enabledLabelValue,
					hyperOpsGitopsNamespaceLabel: gitOpsNamespace.Name,
				}
				Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

				By("Creating the Manager")
				cm, err := manager.New(cfg, manager.Options{
					Metrics:                metricsserver.Options{BindAddress: "0"},
					HealthProbeBindAddress: "0",
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(hyperOpsReconciler.SetupWithManager(cm)).To(Succeed())

				By("Starting the Manager")
				mgrCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go func() {
					defer GinkgoRecover()
					Expect(cm.Start(mgrCtx)).NotTo(HaveOccurred())
				}()

				By("Waiting for the ArgoCD cluster secret created by the watch")
				hostedSecret := &corev1.Secret{}
				Eventually(func() error {
					return k8sClient.Get(ctx, types.NamespacedName{
						Name: hyperOpsControllerBaseName, Namespace: gitOpsNamespace.Name,
					}, hostedSecret)
				}, 30*time.Second, time.Second).Should(Succeed())
				Expect(hostedSecret.Labels).To(HaveKeyWithValue(hyperOpsTypeLabel, hyperOpsTypeHosted))

				cancel()
			})
		})

		Describe("Deleting a HostedCluster", func() {
			It("removes the ArgoCD cluster secrets it owns and the finalizer", func() {
				By("Labeling the HostedCluster with its own namespace as GitOps namespace")
				Expect(k8sClient.Get(ctx, typeNamespaceName, cluster)).To(Succeed())
				cluster.Labels = map[string]string{
					hyperOpsEnabledLabel:         enabledLabelValue,
					hyperOpsGitopsNamespaceLabel: namespace.Name,
				}
				Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

				By("Reconciling the HostedCluster")
				_, err := hyperOpsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
				Expect(err).NotTo(HaveOccurred())

				By("Checking the finalizer and the owned secrets")
				Expect(k8sClient.Get(ctx, typeNamespaceName, cluster)).To(Succeed())
				Expect(controllerutil.ContainsFinalizer(cluster, hyperOpsFinalizer)).To(BeTrue())

				inClusterSecret := &corev1.Secret{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: localClusterName, Namespace: namespace.Name,
				}, inClusterSecret)).To(Succeed())
				Expect(metav1.IsControlledBy(inClusterSecret, cluster)).To(BeTrue())

				hostedSecret := &corev1.Secret{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: hyperOpsControllerBaseName, Namespace: namespace.Name,
				}, hostedSecret)).To(Succeed())
				Expect(metav1.IsControlledBy(hostedSecret, cluster)).To(BeTrue())

				By("Deleting the HostedCluster and running the cleanup")
				Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
				_, err = hyperOpsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
				Expect(err).NotTo(HaveOccurred())

				By("Checking the secrets and the HostedCluster are gone")
				Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
					Name: localClusterName, Namespace: namespace.Name}, &corev1.Secret{}))).To(BeTrue())
				Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
					Name: hyperOpsControllerBaseName, Namespace: namespace.Name}, &corev1.Secret{}))).To(BeTrue())
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, typeNamespaceName, &hypershiftv1beta1.HostedCluster{}))
				}, 10*time.Second, time.Second).Should(BeTrue())
			})

			It("keeps the shared in-cluster secret when it is not owned", func() {
				By("Labeling the HostedCluster with a shared GitOps namespace")
				Expect(k8sClient.Get(ctx, typeNamespaceName, cluster)).To(Succeed())
				cluster.Labels = map[string]string{
					hyperOpsEnabledLabel:         enabledLabelValue,
					hyperOpsGitopsNamespaceLabel: gitOpsNamespace.Name,
				}
				Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

				By("Reconciling the HostedCluster")
				_, err := hyperOpsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
				Expect(err).NotTo(HaveOccurred())

				By("Deleting the HostedCluster and running the cleanup")
				Expect(k8sClient.Get(ctx, typeNamespaceName, cluster)).To(Succeed())
				Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
				_, err = hyperOpsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
				Expect(err).NotTo(HaveOccurred())

				By("Checking the HostedCluster secret is gone but the shared secret is kept")
				Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
					Name: hyperOpsControllerBaseName, Namespace: gitOpsNamespace.Name}, &corev1.Secret{}))).To(BeTrue())
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: localClusterName, Namespace: gitOpsNamespace.Name}, &corev1.Secret{})).To(Succeed())
			})

			It("leaves a secret with the same name that it does not manage in place", func() {
				By("Labeling the HostedCluster")
				Expect(k8sClient.Get(ctx, typeNamespaceName, cluster)).To(Succeed())
				cluster.Labels = map[string]string{
					hyperOpsEnabledLabel:         enabledLabelValue,
					hyperOpsGitopsNamespaceLabel: gitOpsNamespace.Name,
				}
				Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

				By("Reconciling the HostedCluster")
				_, err := hyperOpsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
				Expect(err).NotTo(HaveOccurred())

				By("Replacing the ArgoCD cluster secret with one that is not managed by hyper-ops")
				Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
					Name: hyperOpsControllerBaseName, Namespace: gitOpsNamespace.Name,
				}})).To(Succeed())
				Expect(k8sClient.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      hyperOpsControllerBaseName,
						Namespace: gitOpsNamespace.Name,
						Labels:    map[string]string{argoCDSecretTypeLabel: argoCDSecretTypeCluster},
					},
				})).To(Succeed())

				By("Deleting the HostedCluster and running the cleanup")
				Expect(k8sClient.Get(ctx, typeNamespaceName, cluster)).To(Succeed())
				Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
				_, err = hyperOpsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespaceName})
				Expect(err).NotTo(HaveOccurred())

				By("Checking that the foreign secret was left in place")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: hyperOpsControllerBaseName, Namespace: gitOpsNamespace.Name}, &corev1.Secret{})).To(Succeed())
			})
		})

		Describe("Manager RBAC", func() {
			It("grants access to clusterrolebindings", func() {
				raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
				Expect(err).NotTo(HaveOccurred())
				role := &rbacv1.ClusterRole{}
				Expect(utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(role)).To(Succeed())

				index := slices.IndexFunc(role.Rules, func(rule rbacv1.PolicyRule) bool {
					return slices.Contains(rule.Resources, "clusterrolebindings")
				})
				Expect(index).To(BeNumerically(">=", 0), "config/rbac/role.yaml must grant access to clusterrolebindings")
				rule := role.Rules[index]
				Expect(rule.APIGroups).To(ContainElement("rbac.authorization.k8s.io"))
				Expect(rule.Verbs).To(ConsistOf("create", "delete", "get", "list", "patch", "update", "watch"))
			})
		})
	})
})

// newHostedCluster returns a HostedCluster that satisfies the API validation.
func newHostedCluster(name, namespace string) *hypershiftv1beta1.HostedCluster {
	return &hypershiftv1beta1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: hypershiftv1beta1.HostedClusterSpec{
			// The current HostedCluster API requires a pull secret reference,
			// managed etcd storage and the full set of published services.
			PullSecret: corev1.LocalObjectReference{
				Name: "pull-secret",
			},
			Release: hypershiftv1beta1.Release{
				Image: "quay.io/openshift-release-dev/ocp-release:4.8.0-fc.0-x86_64",
			},
			Etcd: hypershiftv1beta1.EtcdSpec{
				ManagementType: hypershiftv1beta1.Managed,
				Managed: &hypershiftv1beta1.ManagedEtcdSpec{
					Storage: hypershiftv1beta1.ManagedEtcdStorageSpec{
						Type: hypershiftv1beta1.PersistentVolumeEtcdStorage,
					},
				},
			},
			Networking: hypershiftv1beta1.ClusterNetworking{
				NetworkType: hypershiftv1beta1.OVNKubernetes,
				ClusterNetwork: []hypershiftv1beta1.ClusterNetworkEntry{
					{
						CIDR:       *ipnet.MustParseCIDR("10.0.0.0/8"),
						HostPrefix: 8},
				},
			},
			Platform: hypershiftv1beta1.PlatformSpec{
				Type: hypershiftv1beta1.KubevirtPlatform,
			},
			Services: []hypershiftv1beta1.ServicePublishingStrategyMapping{
				{
					Service: hypershiftv1beta1.APIServer,
					ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{
						Type: hypershiftv1beta1.LoadBalancer,
					},
				},
				{
					Service: hypershiftv1beta1.OAuthServer,
					ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{
						Type: hypershiftv1beta1.Route,
					},
				},
				{
					Service: hypershiftv1beta1.Konnectivity,
					ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{
						Type: hypershiftv1beta1.Route,
					},
				},
				{
					Service: hypershiftv1beta1.Ignition,
					ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{
						Type: hypershiftv1beta1.Route,
					},
				},
			},
		},
	}
}

// generateKubeConfig converts the rest.Config of the test environment into a
// kubeconfig.
func generateKubeConfig(cfg *rest.Config) ([]byte, error) {
	kubeConfig := clientcmdapi.NewConfig()
	kubeConfig.Clusters["cluster"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
	}
	kubeConfig.AuthInfos["admin"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
	}
	kubeConfig.Contexts["admin"] = &clientcmdapi.Context{
		Cluster:  "cluster",
		AuthInfo: "admin",
	}
	kubeConfig.CurrentContext = "admin"
	// return the kubeconfig as a string
	return clientcmd.Write(*kubeConfig)
}
