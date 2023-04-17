package controllers

import (
	"context"

	configv1 "github.com/openshift/api/config/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// CreateOrUpdateWithRetries creates or updates the given object in the Kubernetes with retries
func CreateOrUpdateWithRetries(
	ctx context.Context,
	c client.Client,
	obj client.Object,
	f controllerutil.MutateFn,
) (controllerutil.OperationResult, error) {
	var operationResult controllerutil.OperationResult
	updateErr := wait.ExponentialBackoff(retry.DefaultBackoff, func() (ok bool, err error) {
		operationResult, err = controllerutil.CreateOrUpdate(ctx, c, obj, f)
		if err == nil {
			return true, nil
		}
		if !apierrors.IsConflict(err) {
			return false, err
		}
		return false, nil
	})
	return operationResult, updateErr
}

func GetClientForCluster(configBytes []byte) (client.Client, error) {
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(configBytes)

	if err != nil {
		return nil, err
	}
	err = configv1.AddToScheme(scheme.Scheme)
	if err != nil {
		return nil, err
	}

	return client.New(restConfig, client.Options{Scheme: scheme.Scheme})
}
