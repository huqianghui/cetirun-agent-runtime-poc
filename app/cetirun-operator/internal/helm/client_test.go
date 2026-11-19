// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"testing"
)

func TestChartReference_Validate(t *testing.T) {
	tests := []struct {
		name    string
		ref     ChartReference
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid OCI reference",
			ref: ChartReference{
				RepoURL: "oci://cetirun.azurecr.io/helm/cetirun",
				Version: "0.1.28",
			},
			wantErr: false,
		},
		{
			name:    "missing RepoURL",
			ref:     ChartReference{Version: "0.1.28"},
			wantErr: true,
			errMsg:  "RepoURL must be specified",
		},
		{
			name:    "missing Version",
			ref:     ChartReference{RepoURL: "oci://cetirun.azurecr.io/helm/cetirun"},
			wantErr: true,
			errMsg:  "Version must be specified",
		},
		{
			name:    "invalid URL scheme",
			ref:     ChartReference{RepoURL: "https://cetirun.azurecr.io/helm/cetirun", Version: "0.1.28"},
			wantErr: true,
			errMsg:  "OCI registry URL must start with oci://",
		},
		{
			name:    "empty",
			ref:     ChartReference{},
			wantErr: true,
			errMsg:  "RepoURL must be specified",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.ref.Validate()
			if tt.wantErr {
				if err == nil {
					t.Errorf("Validate() error = nil, want error")
					return
				}
				if err.Error() != tt.errMsg {
					t.Errorf("Validate() error = %q, want %q", err.Error(), tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("Validate() unexpected error = %v", err)
				}
			}
		})
	}
}

func TestNewClient(t *testing.T) {
	ref := ChartReference{
		RepoURL: "oci://cetirun.azurecr.io/helm/cetirun",
		Version: "0.1.28",
	}
	client := NewClient(ref)
	if client == nil {
		t.Fatal("NewClient() returned nil")
	}
	if client.chartRef.RepoURL != ref.RepoURL {
		t.Errorf("chartRef.RepoURL = %v, want %v", client.chartRef.RepoURL, ref.RepoURL)
	}
	if client.chartRef.Version != ref.Version {
		t.Errorf("chartRef.Version = %v, want %v", client.chartRef.Version, ref.Version)
	}
}
