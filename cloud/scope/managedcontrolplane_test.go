/*
Copyright 2026 The Kubernetes Authors.

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

package scope

import (
	"testing"

	"k8s.io/utils/ptr"
	infrav1 "sigs.k8s.io/cluster-api-provider-gcp/api/v1beta1"
	infrav1exp "sigs.k8s.io/cluster-api-provider-gcp/exp/api/v1beta1"
)

func TestManagedControlPlaneScopeNetworkQualification(t *testing.T) {
	tests := []struct {
		name               string
		project            string
		hostProject        *string
		networkName        string
		subnetworkName     string
		region             string
		wantIsSharedVpc    bool
		wantNetworkName    string
		wantSubnetworkName string
	}{
		{
			name:               "non-shared VPC keeps bare names",
			project:            "my-project",
			hostProject:        nil,
			networkName:        "vpc-a",
			subnetworkName:     "vpc-a-us-central1",
			region:             "us-central1",
			wantIsSharedVpc:    false,
			wantNetworkName:    "vpc-a",
			wantSubnetworkName: "vpc-a-us-central1",
		},
		{
			name:               "host project equal to project is not shared VPC",
			project:            "my-project",
			hostProject:        ptr.To("my-project"),
			networkName:        "vpc-a",
			subnetworkName:     "vpc-a-us-central1",
			region:             "us-central1",
			wantIsSharedVpc:    false,
			wantNetworkName:    "vpc-a",
			wantSubnetworkName: "vpc-a-us-central1",
		},
		{
			name:               "shared VPC qualifies network and subnetwork with host project",
			project:            "my-project",
			hostProject:        ptr.To("my-shared-vpc-project"),
			networkName:        "my-network",
			subnetworkName:     "my-subnet",
			region:             "us-central1",
			wantIsSharedVpc:    true,
			wantNetworkName:    "projects/my-shared-vpc-project/global/networks/my-network",
			wantSubnetworkName: "projects/my-shared-vpc-project/regions/us-central1/subnetworks/my-subnet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &ManagedControlPlaneScope{
				GCPManagedControlPlane: &infrav1exp.GCPManagedControlPlane{
					Spec: infrav1exp.GCPManagedControlPlaneSpec{
						GCPManagedControlPlaneClassSpec: infrav1exp.GCPManagedControlPlaneClassSpec{
							Project:  tt.project,
							Location: tt.region,
						},
					},
				},
				GCPManagedCluster: &infrav1exp.GCPManagedCluster{
					Spec: infrav1exp.GCPManagedClusterSpec{
						Project: tt.project,
						Region:  tt.region,
						Network: infrav1.NetworkSpec{
							Name:        ptr.To(tt.networkName),
							HostProject: tt.hostProject,
						},
					},
				},
			}

			if got := s.IsSharedVpc(); got != tt.wantIsSharedVpc {
				t.Errorf("IsSharedVpc() = %v, want %v", got, tt.wantIsSharedVpc)
			}
			if got := s.NetworkFullName(); got != tt.wantNetworkName {
				t.Errorf("NetworkFullName() = %q, want %q", got, tt.wantNetworkName)
			}
			if got := s.SubnetworkFullName(tt.subnetworkName); got != tt.wantSubnetworkName {
				t.Errorf("SubnetworkFullName(%q) = %q, want %q", tt.subnetworkName, got, tt.wantSubnetworkName)
			}
		})
	}
}

func TestManagedControlPlaneScopeNetworkFullNameEmptyName(t *testing.T) {
	s := &ManagedControlPlaneScope{
		GCPManagedControlPlane: &infrav1exp.GCPManagedControlPlane{
			Spec: infrav1exp.GCPManagedControlPlaneSpec{
				GCPManagedControlPlaneClassSpec: infrav1exp.GCPManagedControlPlaneClassSpec{
					Project:  "my-project",
					Location: "us-central1",
				},
			},
		},
		GCPManagedCluster: &infrav1exp.GCPManagedCluster{
			Spec: infrav1exp.GCPManagedClusterSpec{
				Project: "my-project",
				Region:  "us-central1",
				Network: infrav1.NetworkSpec{
					Name:        nil,
					HostProject: ptr.To("my-shared-vpc-project"),
				},
			},
		},
	}

	if got := s.NetworkFullName(); got != "" {
		t.Errorf("NetworkFullName() with nil Name = %q, want empty string", got)
	}
	if got := s.SubnetworkFullName(""); got != "" {
		t.Errorf("SubnetworkFullName(\"\") = %q, want empty string", got)
	}
}
