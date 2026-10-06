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
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"cloud.google.com/go/container/apiv1/containerpb"
	"cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	infrav1exp "sigs.k8s.io/cluster-api-provider-gcp/exp/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/cluster-api/util/secret"
)

const (
	// GkeScope is the scope to request when generating access token.
	GkeScope = "https://www.googleapis.com/auth/cloud-platform"
	// GkeEmailScope lets the Kubernetes API server resolve the token identity.
	GkeEmailScope                      = "https://www.googleapis.com/auth/userinfo.email"
	kubeconfigTokenExpiryAnnotation    = "infrastructure.cluster.x-k8s.io/kubeconfig-token-expiry"
	kubeconfigServiceAccountAnnotation = "infrastructure.cluster.x-k8s.io/kubeconfig-service-account"
	kubeconfigRefreshBeforeExpiry      = 5 * time.Minute
)

func (s *Service) reconcileKubeconfig(ctx context.Context, cluster *containerpb.Cluster, log *logr.Logger) (time.Duration, error) {
	log.Info("Reconciling kubeconfig")
	clusterRef := types.NamespacedName{Name: s.scope.Cluster.Name, Namespace: s.scope.Cluster.Namespace}
	configSecret, err := secret.GetFromNamespacedName(ctx, s.scope.Client(), clusterRef, secret.Kubeconfig)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("getting kubeconfig secret %s: %w", clusterRef, err)
		}
		configSecret, err = s.createCAPIKubeconfigSecret(ctx, cluster, &clusterRef, log)
		if err != nil {
			return 0, fmt.Errorf("creating kubeconfig secret: %w", err)
		}
	} else if s.kubeconfigNeedsRefresh(configSecret) {
		if err := s.updateCAPIKubeconfigSecret(ctx, configSecret); err != nil {
			return 0, fmt.Errorf("updating kubeconfig secret: %w", err)
		}
	}
	expiry, err := time.Parse(time.RFC3339, configSecret.Annotations[kubeconfigTokenExpiryAnnotation])
	if err != nil {
		return 0, fmt.Errorf("reading kubeconfig token expiry: %w", err)
	}
	return expiry.Sub(s.now()) - kubeconfigRefreshBeforeExpiry, nil
}

// kubeconfigNeedsRefresh also migrates Secrets created by earlier CAPG versions.
func (s *Service) kubeconfigNeedsRefresh(configSecret *corev1.Secret) bool {
	if configSecret.Annotations[kubeconfigServiceAccountAnnotation] != s.scope.GCPManagedControlPlane.Spec.KubeconfigServiceAccountEmail {
		return true
	}
	expiry, err := time.Parse(time.RFC3339, configSecret.Annotations[kubeconfigTokenExpiryAnnotation])
	return err != nil || !s.now().Add(kubeconfigRefreshBeforeExpiry).Before(expiry)
}

func (s *Service) setKubeconfigTokenAnnotations(configSecret *corev1.Secret, token *credentialspb.GenerateAccessTokenResponse) {
	if configSecret.Annotations == nil {
		configSecret.Annotations = map[string]string{}
	}
	configSecret.Annotations[kubeconfigTokenExpiryAnnotation] = token.GetExpireTime().AsTime().UTC().Format(time.RFC3339)
	configSecret.Annotations[kubeconfigServiceAccountAnnotation] = s.scope.GCPManagedControlPlane.Spec.KubeconfigServiceAccountEmail
}

func (s *Service) reconcileAdditionalKubeconfigs(ctx context.Context, cluster *containerpb.Cluster, log *logr.Logger) error {
	log.Info("Reconciling additional kubeconfig")
	clusterRef := types.NamespacedName{
		Name:      s.scope.Cluster.Name + "-user",
		Namespace: s.scope.Cluster.Namespace,
	}

	// Create the additional kubeconfig for users. This doesn't need updating on every sync
	_, err := secret.GetFromNamespacedName(ctx, s.scope.Client(), clusterRef, secret.Kubeconfig)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("getting kubeconfig (user) secret %s: %w", clusterRef, err)
		}

		createErr := s.createUserKubeconfigSecret(
			ctx,
			cluster,
			&clusterRef,
		)
		if createErr != nil {
			return fmt.Errorf("creating additional kubeconfig secret: %w", err)
		}
	}

	return nil
}

func (s *Service) createUserKubeconfigSecret(ctx context.Context, cluster *containerpb.Cluster, clusterRef *types.NamespacedName) error {
	controllerOwnerRef := *metav1.NewControllerRef(s.scope.GCPManagedControlPlane, infrav1exp.GroupVersion.WithKind("GCPManagedControlPlane"))

	contextName := s.getKubeConfigContextName(false)

	cfg, err := s.createBaseKubeConfig(contextName, cluster)
	if err != nil {
		return fmt.Errorf("creating base kubeconfig: %w", err)
	}

	execConfig := &api.ExecConfig{
		APIVersion:         "client.authentication.k8s.io/v1beta1",
		Command:            "gke-gcloud-auth-plugin",
		InstallHint:        "Install gke-gcloud-auth-plugin for use with kubectl by following\n		https://cloud.google.com/blog/products/containers-kubernetes/kubectl-auth-changes-in-gke",
		ProvideClusterInfo: true,
	}
	cfg.AuthInfos = map[string]*api.AuthInfo{
		contextName: {
			Exec: execConfig,
		},
	}

	out, err := clientcmd.Write(*cfg)
	if err != nil {
		return fmt.Errorf("serialize kubeconfig to yaml: %w", err)
	}

	kubeconfigSecret := kubeconfig.GenerateSecretWithOwner(*clusterRef, out, controllerOwnerRef)
	if err := s.scope.Client().Create(ctx, kubeconfigSecret); err != nil {
		return fmt.Errorf("creating secret: %w", err)
	}

	return nil
}

func (s *Service) createCAPIKubeconfigSecret(ctx context.Context, cluster *containerpb.Cluster, clusterRef *types.NamespacedName, log *logr.Logger) (*corev1.Secret, error) {
	controllerOwnerRef := *metav1.NewControllerRef(s.scope.GCPManagedControlPlane, infrav1exp.GroupVersion.WithKind("GCPManagedControlPlane"))

	contextName := s.getKubeConfigContextName(false)

	cfg, err := s.createBaseKubeConfig(contextName, cluster)
	if err != nil {
		log.Error(err, "failed creating base config")
		return nil, fmt.Errorf("creating base kubeconfig: %w", err)
	}

	token, err := s.generateToken(ctx)
	if err != nil {
		log.Error(err, "failed generating token")
		return nil, err
	}
	cfg.AuthInfos = map[string]*api.AuthInfo{
		contextName: {
			Token: token.GetAccessToken(),
		},
	}

	out, err := clientcmd.Write(*cfg)
	if err != nil {
		log.Error(err, "failed serializing kubeconfig to yaml")
		return nil, fmt.Errorf("serialize kubeconfig to yaml: %w", err)
	}

	kubeconfigSecret := kubeconfig.GenerateSecretWithOwner(*clusterRef, out, controllerOwnerRef)
	s.setKubeconfigTokenAnnotations(kubeconfigSecret, token)
	if err := s.scope.Client().Create(ctx, kubeconfigSecret); err != nil {
		log.Error(err, "failed creating secret")
		return nil, fmt.Errorf("creating secret: %w", err)
	}

	return kubeconfigSecret, nil
}

func (s *Service) updateCAPIKubeconfigSecret(ctx context.Context, configSecret *corev1.Secret) error {
	data, ok := configSecret.Data[secret.KubeconfigDataName]
	if !ok {
		return errors.Errorf("missing key %q in secret data", secret.KubeconfigDataName)
	}

	config, err := clientcmd.Load(data)
	if err != nil {
		return errors.Wrap(err, "failed to convert kubeconfig Secret into a clientcmdapi.Config")
	}

	token, err := s.generateToken(ctx)
	if err != nil {
		return err
	}

	contextName := s.getKubeConfigContextName(false)
	authInfo := config.AuthInfos[contextName]
	if authInfo == nil {
		return errors.Errorf("missing auth info %q in kubeconfig Secret", contextName)
	}
	authInfo.Token = token.GetAccessToken()

	out, err := clientcmd.Write(*config)
	if err != nil {
		return errors.Wrap(err, "failed to serialize config to yaml")
	}

	configSecret.Data[secret.KubeconfigDataName] = out
	s.setKubeconfigTokenAnnotations(configSecret, token)

	err = s.scope.Client().Update(ctx, configSecret)
	if err != nil {
		return fmt.Errorf("updating kubeconfig secret: %w", err)
	}

	return nil
}

func (s *Service) getKubeConfigContextName(isUser bool) string {
	contextName := fmt.Sprintf("gke_%s_%s_%s", s.scope.GCPManagedControlPlane.Spec.Project, s.scope.GCPManagedControlPlane.Spec.Location, s.scope.ClusterName())
	if isUser {
		contextName += "-user"
	}
	return contextName
}

func (s *Service) createBaseKubeConfig(contextName string, cluster *containerpb.Cluster) (*api.Config, error) {
	certData, err := base64.StdEncoding.DecodeString(cluster.GetMasterAuth().GetClusterCaCertificate())
	if err != nil {
		return nil, fmt.Errorf("decoding cluster CA cert: %w", err)
	}
	cfg := &api.Config{
		APIVersion: api.SchemeGroupVersion.Version,
		Clusters: map[string]*api.Cluster{
			contextName: {
				Server:                   "https://" + cluster.GetEndpoint(),
				CertificateAuthorityData: certData,
			},
		},
		Contexts: map[string]*api.Context{
			contextName: {
				Cluster:  contextName,
				AuthInfo: contextName,
			},
		},
		CurrentContext: contextName,
	}

	return cfg, nil
}

// tokenEmailResolver resolves the service account email used for token generation.
type tokenEmailResolver interface {
	Email(ctx context.Context) (string, error)
}

// credentialEmailResolver uses an explicit service account email from loaded credentials.
type credentialEmailResolver struct {
	email string
}

func (r credentialEmailResolver) Email(_ context.Context) (string, error) {
	return r.email, nil
}

// metadataEmailResolver discovers the bound service account email from the GKE
// metadata server, used when running under Workload Identity Federation.
type metadataEmailResolver struct{}

func (r metadataEmailResolver) Email(ctx context.Context) (string, error) {
	return metadata.EmailWithContext(ctx, "default")
}

func (s *Service) generateToken(ctx context.Context) (*credentialspb.GenerateAccessTokenResponse, error) {
	email, err := s.emailResolver.Email(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving service account email: %w", err)
	}
	if !strings.HasSuffix(email, ".gserviceaccount.com") {
		return nil, errors.Errorf("identity %q is not a Google service account email; configure kubeconfigServiceAccountEmail for direct Workload Identity access", email)
	}
	req := &credentialspb.GenerateAccessTokenRequest{
		Name:  "projects/-/serviceAccounts/" + email,
		Scope: []string{GkeScope, GkeEmailScope},
	}
	resp, err := s.mintToken(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("error generating access token for %q: %w", email, err)
	}

	if resp.GetAccessToken() == "" || resp.GetExpireTime() == nil {
		return nil, errors.New("IAM returned an access token without a value or expiration")
	}
	if err := resp.GetExpireTime().CheckValid(); err != nil {
		return nil, fmt.Errorf("invalid access token expiration: %w", err)
	}
	if !s.now().Add(kubeconfigRefreshBeforeExpiry).Before(resp.GetExpireTime().AsTime()) {
		return nil, errors.New("IAM returned an access token too close to expiration")
	}
	return resp, nil
}
