package controller

import (
	"context"
	"fmt"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// CreateOrUpdateWithRetries creates or updates the given object and retries the
// operation when the API server reports a conflict.
//
// Conflicts are retried in process with the default backoff; all other errors are
// returned immediately and enriched with the identity of the object.
func CreateOrUpdateWithRetries(
	ctx context.Context,
	c client.Client,
	obj client.Object,
	f controllerutil.MutateFn) (controllerutil.OperationResult, error) {
	var operationResult controllerutil.OperationResult
	logger := log.FromContext(ctx)
	updateErr := wait.ExponentialBackoff(retry.DefaultBackoff, func() (ok bool, err error) {
		operationResult, err = controllerutil.CreateOrUpdate(ctx, c, obj, f)
		if err == nil {
			return true, nil
		}
		if !apierrors.IsConflict(err) {
			return false, err
		}
		// The object is re-read by CreateOrUpdate on the next attempt.
		logger.V(1).Info("retrying create or update after conflict", "object", objectDescription(obj))
		return false, nil
	})
	if updateErr != nil {
		return operationResult, fmt.Errorf("creating or updating %s: %w", objectDescription(obj), updateErr)
	}
	return operationResult, nil
}

// GetClientForCluster returns a client for the cluster described by the given
// kubeconfig.
func GetClientForCluster(configBytes []byte) (client.Client, error) {
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(configBytes)
	if err != nil {
		return nil, fmt.Errorf("building REST config from kubeconfig: %w", err)
	}
	err = configv1.AddToScheme(scheme.Scheme)
	if err != nil {
		return nil, fmt.Errorf("registering OpenShift config types: %w", err)
	}

	return client.New(restConfig, client.Options{Scheme: scheme.Scheme})
}

// objectDescription identifies an object for logs and wrapped errors. It never
// contains the object content, which would leak Secret data into the logs.
func objectDescription(obj client.Object) string {
	if obj == nil {
		return "<nil>"
	}
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		kind = strings.TrimPrefix(fmt.Sprintf("%T", obj), "*")
	}
	return fmt.Sprintf("%s %s", kind, client.ObjectKeyFromObject(obj))
}

// mergeStringMaps returns a new map that contains current and desired. Neither
// input map is modified.
func mergeStringMaps(current, desired map[string]string) map[string]string {
	merged := make(map[string]string, len(current)+len(desired))
	for k, v := range current {
		merged[k] = v
	}
	for k, v := range desired {
		merged[k] = v
	}
	return merged
}
