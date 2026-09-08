// Package v1alpha1 contains the API schema definition for the truenas.watteel.com
// v1alpha1 API group.
//
// +kubebuilder:object:generate=true
// +groupName=truenas.watteel.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "truenas.watteel.com", Version: "v1alpha1"}

	// SchemeBuilder registers the Go types with a scheme.
	//
	// apimachinery's builder rather than controller-runtime's, which is
	// deprecated for the reason that applies here: an api package should be
	// cheap to import, so it should not pull controller-runtime in behind it.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &TrueNASCSIDriver{}, &TrueNASCSIDriverList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
