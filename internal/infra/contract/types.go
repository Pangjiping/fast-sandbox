// Package contract defines runtime-neutral Infra Component observations that
// can cross the Fastlet/runtime adapter boundary.
package contract

import infracatalog "fast-sandbox/internal/catalog/infra"

type ServiceEndpoint struct {
	Component string                      `json:"component"`
	Protocol  string                      `json:"protocol"`
	Port      uint32                      `json:"port"`
	Readiness infracatalog.ReadinessProbe `json:"readiness"`
}

type ComponentDiagnostic struct {
	Component string `json:"component"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
}
