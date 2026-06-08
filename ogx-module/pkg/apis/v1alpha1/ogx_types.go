package v1alpha1

import (
	common "github.com/opendatahub-io/odh-platform-utilities/api/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	OGXComponentName = "ogx"
	OGXInstanceName  = "default-ogx"
	OGXKind          = "OGX"
)

// Compile-time interface assertion for the shared platform contract.
var _ common.PlatformObject = (*OGX)(nil)

// OGXDistribution captures the platform/distribution alignment reported by the module.
// This mirrors the onboarding contract even though the shared common.Status does not
// currently embed a distribution field.
type OGXDistribution struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// OGXStatus defines the observed state of OGX.
type OGXStatus struct {
	common.Status                 `json:",inline"`
	common.ComponentReleaseStatus `json:",inline"`

	// Distribution reports the platform context the module is currently aligned with.
	Distribution OGXDistribution `json:"distribution,omitempty"`
}

// OGXSpec defines the desired state of OGX.
//
// The full module-facing spec surface is added in later tasks. This initial scaffold
// preserves the ODH-facing kind/GVK and status contract without guessing operand-level
// configuration prematurely.
type OGXSpec struct{}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,path=ogxs
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default-ogx'",message="OGX name must be default-ogx"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,description="Reason"
// +kubebuilder:printcolumn:name="Distribution",type=string,JSONPath=`.status.distribution.name`,description="Distribution"

// OGX is the Schema for the ogxs API.
type OGX struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OGXSpec   `json:"spec,omitempty"`
	Status OGXStatus `json:"status,omitempty"`
}

func (c *OGX) GetStatus() *common.Status {
	return &c.Status.Status
}

func (c *OGX) GetConditions() []common.Condition {
	conditions := make([]common.Condition, len(c.Status.Conditions))
	copy(conditions, c.Status.Conditions)
	return conditions
}

func (c *OGX) SetConditions(conditions []common.Condition) {
	c.Status.Conditions = make([]common.Condition, len(conditions))
	copy(c.Status.Conditions, conditions)
}

func (c *OGX) GetReleaseStatus() *common.ComponentReleaseStatus {
	return &c.Status.ComponentReleaseStatus
}

func (c *OGX) SetReleaseStatus(status common.ComponentReleaseStatus) {
	c.Status.ComponentReleaseStatus = status
}

// +kubebuilder:object:root=true

// OGXList contains a list of OGX.
type OGXList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OGX `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OGX{}, &OGXList{})
}
