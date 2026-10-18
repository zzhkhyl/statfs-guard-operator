// Package v1alpha1 contains the API schema for the statfs-guard operator.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the API group owned by this operator.
const GroupName = "storage.statfsguard.io"

// GroupVersion is the only served version.
var GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

var (
	// SchemeBuilder collects the scheme registration functions.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme registers this group's types on a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &CephFSStatfsReport{}, &CephFSStatfsReportList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
