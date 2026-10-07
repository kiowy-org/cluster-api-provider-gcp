/*
Copyright 2023 The Kubernetes Authors.

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

package clusters

import (
	"context"
	"time"

	"cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	"sigs.k8s.io/cluster-api-provider-gcp/cloud"
	"sigs.k8s.io/cluster-api-provider-gcp/cloud/scope"
)

// Service implements clusters reconciler.
type Service struct {
	scope         *scope.ManagedControlPlaneScope
	emailResolver tokenEmailResolver
	mintToken     func(context.Context, *credentialspb.GenerateAccessTokenRequest) (*credentialspb.GenerateAccessTokenResponse, error)
	now           func() time.Time
}

var _ cloud.ReconcilerWithResult = &Service{}

// New returns Service from given scope.
func New(s *scope.ManagedControlPlaneScope) *Service {
	var resolver tokenEmailResolver
	if email := s.GCPManagedControlPlane.Spec.KubeconfigServiceAccountEmail; email != "" {
		resolver = credentialEmailResolver{email: email}
	} else if cred := s.GetCredential(); cred != nil {
		resolver = credentialEmailResolver{email: cred.ClientEmail}
	} else {
		resolver = metadataEmailResolver{}
	}
	return &Service{
		scope:         s,
		emailResolver: resolver,
		now:           time.Now,
		mintToken: func(ctx context.Context, req *credentialspb.GenerateAccessTokenRequest) (*credentialspb.GenerateAccessTokenResponse, error) {
			return s.CredentialsClient().GenerateAccessToken(ctx, req)
		},
	}
}
