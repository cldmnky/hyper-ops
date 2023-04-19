package controllers

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/openshift/hypershift/api/util/ipnet"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/v1beta1"
)

var _ = Describe("Hyper-Ops controller", func() {
	Context("hyper-ops controller test", func() {
		const hyperOpsControllerBaseName = "test-hyperops"
		var (
			typeNamespaceName           = types.NamespacedName{}
			hyperOpsControllerNameSpace string
			namespace                   *corev1.Namespace
			gitOpsNamespace             *corev1.Namespace
			defaultGitOpsNamespace      *corev1.Namespace
			hyperOpsReconciler          *HyperOpsReconciler
			cluster                     *hypershiftv1beta1.HostedCluster
		)

		ctx := context.Background()

		BeforeEach(func() {
			By("Creating the Namespaces to perform the tests")
			hyperOpsControllerNameSpace = fmt.Sprintf("%s-%d", hyperOpsControllerBaseName, time.Now().UnixMilli())
			typeNamespaceName = types.NamespacedName{Name: hyperOpsControllerBaseName, Namespace: hyperOpsControllerNameSpace}
			namespace = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      hyperOpsControllerNameSpace,
					Namespace: hyperOpsControllerNameSpace,
				},
			}
			err := k8sClient.Create(ctx, namespace)
			Expect(err).To(Not(HaveOccurred()))
			gitOpsNamespace = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("openshift-gitops-%d", time.Now().UnixMilli()),
					Namespace: fmt.Sprintf("openshift-gitops-%d", time.Now().UnixMilli()),
				},
			}
			err = k8sClient.Create(ctx, gitOpsNamespace)
			Expect(err).To(Not(HaveOccurred()))
			// create the openshift-gitops namespace if it does not exist
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "openshift-gitops", Namespace: "openshift-gitops"}, defaultGitOpsNamespace)
			if err != nil {
				defaultGitOpsNamespace = &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "openshift-gitops",
						Namespace: "openshift-gitops",
					},
				}
				err = k8sClient.Create(ctx, defaultGitOpsNamespace)
				Expect(err).To(Not(HaveOccurred()))
			}

			Expect(err).To(Not(HaveOccurred()))

			By("Namespaces created")
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: hyperOpsControllerNameSpace, Namespace: hyperOpsControllerNameSpace}, namespace)
			}, time.Second*10, time.Second*2).Should(Succeed())
			By("Creating a new HostedCluster service without label")
			cluster = &hypershiftv1beta1.HostedCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      hyperOpsControllerBaseName,
					Namespace: hyperOpsControllerNameSpace,
				},
				Spec: hypershiftv1beta1.HostedClusterSpec{
					Release: hypershiftv1beta1.Release{
						Image: "quay.io/openshift-release-dev/ocp-release:4.8.0-fc.0-x86_64",
					},
					Etcd: hypershiftv1beta1.EtcdSpec{
						ManagementType: hypershiftv1beta1.Managed,
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
							Service: hypershiftv1beta1.ServiceType(hypershiftv1beta1.APIServer),
							ServicePublishingStrategy: hypershiftv1beta1.ServicePublishingStrategy{
								Type: hypershiftv1beta1.LoadBalancer,
							},
						},
					},
				},
			}

			err = k8sClient.Create(ctx, cluster)
			Expect(err).To(Not(HaveOccurred()))
			By("Waiting for the created cluster to be created")
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespaceName, cluster)
			}, time.Second*10, time.Second*2).Should(Succeed())
			By("Creating a admin kubeconfig secret")
			kc, err := generateKubeConfig(cfg)
			Expect(err).To(Not(HaveOccurred()))
			adminKubeconfigSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-admin-kubeconfig", hyperOpsControllerBaseName),
					Namespace: hyperOpsControllerNameSpace,
				},
				Data: map[string][]byte{
					"kubeconfig": kc,
				},
			}
			err = k8sClient.Create(ctx, adminKubeconfigSecret)
			Expect(err).To(Not(HaveOccurred()))
			By("Waiting for the created admin kubeconfig secret to be created")
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: adminKubeconfigSecret.Name, Namespace: adminKubeconfigSecret.Namespace}, adminKubeconfigSecret)
			}, time.Second*10, time.Second*2).Should(Succeed())
			// Since we do not have controllers running we need to create the token manually
			By("Creating a token secret")
			tokenSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-token", hostedClusterServiceAccountName),
					Namespace: hostedClusterServiceAccountNamespace,
					Annotations: map[string]string{
						corev1.ServiceAccountNameKey: hostedClusterServiceAccountName,
					},
				},
				Data: map[string][]byte{
					corev1.ServiceAccountTokenKey: []byte("token"),
					// ca cert
					"ca.crt": []byte("ca"),
				},
				Type: corev1.SecretTypeServiceAccountToken,
			}
			_, err = CreateOrUpdateWithRetries(ctx, k8sClient, tokenSecret, func() error {
				return nil
			})
			Expect(err).To(Not(HaveOccurred()))
			By("Creating a hyper ops reconciler")
			hyperOpsReconciler = &HyperOpsReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
		})

		AfterEach(func() {
			// TODO(user): Attention if you improve this code by adding other context test you MUST
			// be aware of the current delete namespace limitations. More info: https://book.kubebuilder.io/reference/envtest.html#testing-considerations
			By("Deleting the HostedCluster")
			cluster := &hypershiftv1beta1.HostedCluster{}
			err := k8sClient.Get(ctx, typeNamespaceName, cluster)
			Expect(err).To(Not(HaveOccurred()))
			err = k8sClient.Delete(ctx, cluster)
			Expect(err).To(Not(HaveOccurred()))
			By("Deleting the Namespaces to perform the tests")
			_ = k8sClient.Delete(ctx, namespace)
			_ = k8sClient.Delete(ctx, gitOpsNamespace)
		})
		Describe("HostedClusters", func() {
			Describe("Without the enable label", func() {
				var reconciled chan reconcile.Request
				It("Should not reconcilce a HostedCluster", func() {
					By("Creating the Manager")
					cm, err := manager.New(cfg, manager.Options{})
					Expect(err).NotTo(HaveOccurred())

					By("Creating the HyperOpsReconciler")
					err = hyperOpsReconciler.SetupWithManager(cm)
					Expect(err).NotTo(HaveOccurred())

					By("Starting the Manager")
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					go func() {
						defer GinkgoRecover()
						Expect(cm.Start(ctx)).NotTo(HaveOccurred())
					}()

					// Wait for 10s and check that the HostedCluster is not reconciled (no reconcile event triggered)
					By("Waiting for 10s to check that the HostedCluster is not reconciled")
					Eventually(func() error {
						// check that no reconcile event is triggered in the channel
						select {
						case <-reconciled:
							return fmt.Errorf("reconcile event triggered")
						default:
							return nil
						}
					}, time.Second*3, time.Second*2).Should(Succeed())
					// cancel the context to stop the manager
					cancel()

				})
			})
			Describe("With enable label", func() {
				It("Should reconcilce a HostedCluster", func() {
					By("Labeling the HostedCluster")
					cluster.Labels = map[string]string{
						"hyper-ops.cloudmonkey.org/enabled":          "true",
						"hyper-ops.cloudmonkey.org/gitops-namespace": gitOpsNamespace.Name,
					}

					err := k8sClient.Update(ctx, cluster)
					Expect(err).To(Not(HaveOccurred()))
					By("Waiting for the created cluster to be reconciled")
					Eventually(func() error {
						return k8sClient.Get(ctx, typeNamespaceName, cluster)
					}, time.Second*10, time.Second*2).Should(Succeed())
					By("Reconciling the hosted cluster resource created")
					req := reconcile.Request{
						NamespacedName: typeNamespaceName,
					}
					_, err = hyperOpsReconciler.Reconcile(ctx, req)
					Expect(err).To(Not(HaveOccurred()))

					By("Checking if the secret exists")
					secret := &corev1.Secret{}
					Eventually(func() error {
						return k8sClient.Get(ctx, types.NamespacedName{Name: hyperOpsControllerBaseName, Namespace: gitOpsNamespace.Name}, secret)
					}, time.Second*10, time.Second*2).Should(Succeed())

					By("Updating the labels on the HostedCluster")
					cluster.Labels = map[string]string{
						"hyper-ops.cloudmonkey.org/enabled":          "true",
						"hyper-ops.cloudmonkey.org/gitops-namespace": gitOpsNamespace.Name,
						"hyper-ops.cloudmonkey.org/cluster-name":     "test",
					}
					err = k8sClient.Update(ctx, cluster)
					Expect(err).To(Not(HaveOccurred()))

					By("Reconciling the hosted cluster resource created")
					req = reconcile.Request{
						NamespacedName: typeNamespaceName,
					}
					_, err = hyperOpsReconciler.Reconcile(ctx, req)
					Expect(err).To(Not(HaveOccurred()))

					By("Checking that the secret labels has been updated")
					Eventually(func() error {
						return k8sClient.Get(ctx, types.NamespacedName{Name: hyperOpsControllerBaseName, Namespace: gitOpsNamespace.Name}, secret)
					}, time.Second*10, time.Second*2).Should(Succeed())
					Expect(secret.Labels).To(HaveKeyWithValue("hyper-ops.cloudmonkey.org/cluster-name", "test"))
				})
			})
		})
	})
})

func generateKubeConfig(cfg *rest.Config) ([]byte, error) {
	// convert the rest.Config to a kubeconfig
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
