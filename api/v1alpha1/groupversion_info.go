/*
Copyright 2024.

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

// Package v1alpha1 contains API Schema definitions for the hyper-ops v1alpha1 API group
// +kubebuilder:object:generate=true
// +groupName=hyper-ops.cloudmonkey.org
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects
	GroupVersion = schema.GroupVersion{Group: "hyper-ops.cloudmonkey.org", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	//
	// It is backed by the apimachinery scheme builders, following the pattern
	// recommended by the deprecation notice of the controller-runtime
	// scheme.Builder helper, so that this package only depends on apimachinery.
	SchemeBuilder = &schemeBuilder{groupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// schemeBuilder collects the API objects of one group version and applies them
// to a runtime.Scheme when AddToScheme is called.
type schemeBuilder struct {
	groupVersion schema.GroupVersion
	adders       []func(*runtime.Scheme) error
}

// Register adds the given API objects to the group version of the builder.
func (b *schemeBuilder) Register(objects ...runtime.Object) {
	b.adders = append(b.adders, func(s *runtime.Scheme) error {
		s.AddKnownTypes(b.groupVersion, objects...)
		metav1.AddToGroupVersion(s, b.groupVersion)
		return nil
	})
}

// AddToScheme applies all registered objects to the given scheme.
func (b *schemeBuilder) AddToScheme(s *runtime.Scheme) error {
	for _, add := range b.adders {
		if err := add(s); err != nil {
			return err
		}
	}
	return nil
}
