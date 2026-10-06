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

package clusters

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	credentials "cloud.google.com/go/iam/credentials/apiv1"
	"cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/cluster-api-provider-gcp/cloud/scope"
	infrav1exp "sigs.k8s.io/cluster-api-provider-gcp/exp/api/v1beta1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredentialEmailResolver(t *testing.T) {
	r := credentialEmailResolver{email: "sa@my-project.iam.gserviceaccount.com"}
	email, err := r.Email(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "sa@my-project.iam.gserviceaccount.com", email)
}

func TestMetadataEmailResolver_WIFMode(t *testing.T) {
	const wifEmail = "wif-sa@my-project.iam.gserviceaccount.com"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/service-accounts/default/email") {
			_, _ = w.Write([]byte(wifEmail))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(srv.URL, "http://"))

	email, err := metadataEmailResolver{}.Email(context.Background())
	require.NoError(t, err)
	assert.Equal(t, wifEmail, email)
}

func TestMetadataEmailResolver_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(srv.URL, "http://"))

	_, err := metadataEmailResolver{}.Email(context.Background())
	require.Error(t, err)
}

const testKubeconfigAccount = "kubeconfig@test-project.iam.gserviceaccount.com"

func newKubeconfigService(t *testing.T, account, explicitCredentialEmail string) *Service {
	t.Helper()
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	if explicitCredentialEmail != "" {
		p := filepath.Join(t.TempDir(), "credentials.json")
		require.NoError(t, os.WriteFile(p, []byte(`{"client_email":"`+explicitCredentialEmail+`"}`), 0o600))
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", p)
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, infrav1exp.AddToScheme(scheme))
	cp := &infrav1exp.GCPManagedControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "control-plane", Namespace: "cell", UID: "owner"},
		Spec: infrav1exp.GCPManagedControlPlaneSpec{
			GCPManagedControlPlaneClassSpec: infrav1exp.GCPManagedControlPlaneClassSpec{
				Project: "test-project", Location: "europe-west9", KubeconfigServiceAccountEmail: account,
			},
			ClusterName: "test-gke",
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	scope, err := scope.NewManagedControlPlaneScope(context.Background(), scope.ManagedControlPlaneScopeParams{
		Client:            k8sClient,
		Cluster:           &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "cell"}},
		GCPManagedCluster: &infrav1exp.GCPManagedCluster{}, GCPManagedControlPlane: cp,
		ManagedClusterClient: &container.ClusterManagerClient{},
		TagBindingsClient:    &resourcemanager.TagBindingsClient{}, CredentialsClient: &credentials.IamCredentialsClient{},
	})
	require.NoError(t, err)
	s := New(scope)
	s.now = func() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) }
	return s
}

func TestGenerateToken_ConfiguredAccountOverridesCredentialsAndMetadata(t *testing.T) {
	for _, credentialEmail := range []string{"", "legacy@test-project.iam.gserviceaccount.com"} {
		t.Run(credentialEmail, func(t *testing.T) {
			s := newKubeconfigService(t, testKubeconfigAccount, credentialEmail)
			s.mintToken = func(_ context.Context, req *credentialspb.GenerateAccessTokenRequest) (*credentialspb.GenerateAccessTokenResponse, error) {
				assert.Equal(t, "projects/-/serviceAccounts/"+testKubeconfigAccount, req.GetName())
				assert.ElementsMatch(t, []string{GkeScope, GkeEmailScope}, req.GetScope())
				return &credentialspb.GenerateAccessTokenResponse{AccessToken: "test-token", ExpireTime: timestamppb.New(s.now().Add(time.Hour))}, nil
			}
			token, err := s.generateToken(context.Background())
			require.NoError(t, err)
			assert.Equal(t, "test-token", token.GetAccessToken())
		})
	}
}

func TestGenerateToken_LegacyCredentialAccount(t *testing.T) {
	s := newKubeconfigService(t, "", testKubeconfigAccount)
	s.mintToken = func(_ context.Context, req *credentialspb.GenerateAccessTokenRequest) (*credentialspb.GenerateAccessTokenResponse, error) {
		assert.Equal(t, "projects/-/serviceAccounts/"+testKubeconfigAccount, req.GetName())
		return &credentialspb.GenerateAccessTokenResponse{AccessToken: "legacy", ExpireTime: timestamppb.New(s.now().Add(time.Hour))}, nil
	}
	_, err := s.generateToken(context.Background())
	require.NoError(t, err)
}

func TestGenerateToken_FederatedIdentityRequiresExplicitAccount(t *testing.T) {
	s := newKubeconfigService(t, "", "")
	s.emailResolver = credentialEmailResolver{email: "kiowy-prod-gke-0.svc.id.goog"}
	s.mintToken = func(context.Context, *credentialspb.GenerateAccessTokenRequest) (*credentialspb.GenerateAccessTokenResponse, error) {
		t.Fatal("must not send a workload pool ID to IAM")
		return nil, nil
	}
	_, err := s.generateToken(context.Background())
	require.ErrorContains(t, err, "configure kubeconfigServiceAccountEmail")
}

func TestGenerateToken_RejectsInvalidResponse(t *testing.T) {
	s := newKubeconfigService(t, testKubeconfigAccount, "")
	for _, response := range []*credentialspb.GenerateAccessTokenResponse{
		{},
		{AccessToken: "token"},
		{AccessToken: "token", ExpireTime: timestamppb.New(s.now().Add(-time.Minute))},
		{AccessToken: "token", ExpireTime: timestamppb.New(s.now().Add(time.Minute))},
	} {
		s.mintToken = func(context.Context, *credentialspb.GenerateAccessTokenRequest) (*credentialspb.GenerateAccessTokenResponse, error) {
			return response, nil
		}
		_, err := s.generateToken(context.Background())
		require.Error(t, err)
	}
}

func TestReconcileKubeconfig_RotationRestartAndFailures(t *testing.T) {
	ctx := context.Background()
	s := newKubeconfigService(t, testKubeconfigAccount, "")
	calls := 0
	s.mintToken = func(_ context.Context, req *credentialspb.GenerateAccessTokenRequest) (*credentialspb.GenerateAccessTokenResponse, error) {
		assert.Equal(t, "projects/-/serviceAccounts/"+s.scope.GCPManagedControlPlane.Spec.KubeconfigServiceAccountEmail, req.GetName())
		calls++
		return &credentialspb.GenerateAccessTokenResponse{AccessToken: "token-" + s.now().Format(time.RFC3339), ExpireTime: timestamppb.New(s.now().Add(time.Hour))}, nil
	}
	cluster := &containerpb.Cluster{Endpoint: "10.0.192.7", MasterAuth: &containerpb.MasterAuth{ClusterCaCertificate: base64.StdEncoding.EncodeToString([]byte("certificate"))}}
	log := logr.Discard()
	delay, err := s.reconcileKubeconfig(ctx, cluster, &log)
	require.NoError(t, err)
	assert.Equal(t, 55*time.Minute, delay)
	assert.Equal(t, 1, calls)
	key := types.NamespacedName{Namespace: "cell", Name: "workload-kubeconfig"}
	getSecret := func() *corev1.Secret {
		t.Helper()
		obj := &corev1.Secret{}
		require.NoError(t, s.scope.Client().Get(ctx, key, obj))
		return obj
	}
	original := getSecret()
	config, err := clientcmd.Load(original.Data[secret.KubeconfigDataName])
	require.NoError(t, err)
	assert.Equal(t, "token-"+s.now().Format(time.RFC3339), config.AuthInfos[s.getKubeConfigContextName(false)].Token)
	assert.Equal(t, "owner", string(original.OwnerReferences[0].UID))

	// The expiry annotation survives a new Service instance after a restart.
	restarted := New(s.scope)
	restarted.now, restarted.mintToken = s.now, s.mintToken
	delay, err = restarted.reconcileKubeconfig(ctx, cluster, &log)
	require.NoError(t, err)
	assert.Equal(t, 55*time.Minute, delay)
	assert.Equal(t, 1, calls)
	assert.Equal(t, original.ResourceVersion, getSecret().ResourceVersion)

	now := s.now().Add(55 * time.Minute)
	s.now = func() time.Time { return now }
	delay, err = s.reconcileKubeconfig(ctx, cluster, &log)
	require.NoError(t, err)
	assert.Equal(t, 55*time.Minute, delay)
	assert.Equal(t, 2, calls)
	rotated := getSecret()
	assert.NotEqual(t, original.Data, rotated.Data)

	// Switching target identities triggers rotation without waiting for expiry.
	s.scope.GCPManagedControlPlane.Spec.KubeconfigServiceAccountEmail = "other@test-project.iam.gserviceaccount.com"
	next := New(s.scope)
	next.now, next.mintToken = s.now, s.mintToken
	s = next
	_, err = s.reconcileKubeconfig(ctx, cluster, &log)
	require.NoError(t, err)
	assert.Equal(t, 3, calls)
	assert.Equal(t, s.scope.GCPManagedControlPlane.Spec.KubeconfigServiceAccountEmail, getSecret().Annotations[kubeconfigServiceAccountAnnotation])

	// IAM propagation / permission failures never overwrite the existing Secret.
	now = now.Add(55 * time.Minute)
	wantErr := errors.New("permission denied")
	s.mintToken = func(context.Context, *credentialspb.GenerateAccessTokenRequest) (*credentialspb.GenerateAccessTokenResponse, error) {
		return nil, wantErr
	}
	beforeFailure := getSecret()
	_, err = s.reconcileKubeconfig(ctx, cluster, &log)
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, beforeFailure, getSecret())
}

func TestKubeconfigNeedsRefresh_LegacyAndMalformedAnnotations(t *testing.T) {
	s := newKubeconfigService(t, testKubeconfigAccount, "")
	for _, annotations := range []map[string]string{
		nil,
		{kubeconfigTokenExpiryAnnotation: "invalid"},
		{kubeconfigTokenExpiryAnnotation: s.now().Add(time.Hour).Format(time.RFC3339)},
	} {
		assert.True(t, s.kubeconfigNeedsRefresh(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}))
	}
}
