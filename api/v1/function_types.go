package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// For now, FunctionSpec describes one script runtime in the mesh.
type FunctionSpec struct {
	Language    string      `json:"language"`
	Source      SourceSpec  `json:"source"`
	Runtime     RuntimeSpec `json:"runtime,omitempty"`
	DeployGroup string      `json:"deployGroup,omitempty"`
}

type SourceSpec struct {
	Type       string `json:"type"`
	ConfigMap  string `json:"configMap"`
	Entrypoint string `json:"entrypoint"`
}

type RuntimeSpec struct {
	Replicas  *int32                      `json:"replicas,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

type FunctionStatus struct {
	Phase        string   `json:"phase,omitempty"`
	ServiceURL   string   `json:"serviceURL,omitempty"`
	Functions    []string `json:"functions,omitempty"`
	PodCount     int32    `json:"podCount,omitempty"`
	Remote       string   `json:"remote,omitempty"`
	RemoteFailed []string `json:"remoteFailed,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fn
// +kubebuilder:printcolumn:name="Language",type=string,JSONPath=`.spec.language`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pods",type=integer,JSONPath=`.status.podCount`
// +kubebuilder:printcolumn:name="Remote",type=string,JSONPath=`.status.remote`
type Function struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FunctionSpec   `json:"spec,omitempty"`
	Status FunctionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type FunctionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Function `json:"items"`
}
