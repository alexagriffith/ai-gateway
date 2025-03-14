// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package rotators

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AWSOIDCRotator implements the Rotator interface for AWS OIDC token exchange.
// It manages the lifecycle of temporary AWS credentials obtained through OIDC token
// exchange with AWS STS.
type AWSOIDCRotator struct {
	// client is used for Kubernetes API operations.
	client client.Client
	// kube provides additional Kubernetes API capabilities.
	kube kubernetes.Interface
	// logger is used for structured logging.
	logger logr.Logger
	// stsClient provides AWS STS operations interface.
	stsClient STSClient
	// backendSecurityPolicyName provides name of backend security policy.
	backendSecurityPolicyName string
	// backendSecurityPolicyNamespace provides namespace of backend security policy.
	backendSecurityPolicyNamespace string
	// preRotationWindow specifies how long before expiry to rotate.
	preRotationWindow time.Duration
	// roleArn is the role ARN used to obtain credentials.
	roleArn string
	// region is the AWS region for the credentials.
	region string
}

// NewAWSOIDCRotator creates a new AWS OIDC rotator with the specified configuration.
// It initializes the AWS STS client and sets up the rotation channels.
func NewAWSOIDCRotator(
	ctx context.Context,
	client client.Client,
	stsClient STSClient,
	kube kubernetes.Interface,
	logger logr.Logger,
	backendSecurityPolicyNamespace string,
	backendSecurityPolicyName string,
	preRotationWindow time.Duration,
	roleArn string,
	region string,
) (*AWSOIDCRotator, error) {
	cfg, err := defaultAWSConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	cfg.Region = region

	if proxyURL := os.Getenv("AI_GATEWAY_STS_PROXY_URL"); proxyURL != "" {
		cfg.HTTPClient = &http.Client{
			Transport: &http.Transport{
				Proxy: func(*http.Request) (*url.URL, error) {
					return url.Parse(proxyURL)
				},
			},
		}
	}
	if stsClient == nil {
		stsClient = NewSTSClient(cfg)
	}
	return &AWSOIDCRotator{
		client:                         client,
		kube:                           kube,
		logger:                         logger.WithName("aws-oidc-rotator"),
		stsClient:                      stsClient,
		backendSecurityPolicyNamespace: backendSecurityPolicyNamespace,
		backendSecurityPolicyName:      backendSecurityPolicyName,
		preRotationWindow:              preRotationWindow,
		roleArn:                        roleArn,
		region:                         region,
	}, nil
}

// IsExpired checks if the preRotation time is before the current time.
func (r *AWSOIDCRotator) IsExpired(preRotationExpirationTime time.Time) bool {
	return IsBufferedTimeExpired(0, preRotationExpirationTime)
}

// GetPreRotationTime gets the expiration time minus the preRotation interval or return zero value for time.
func (r *AWSOIDCRotator) GetPreRotationTime(ctx context.Context) (time.Time, error) {
	secret, err := LookupSecret(ctx, r.client, r.backendSecurityPolicyNamespace, GetBSPSecretName(r.backendSecurityPolicyName))
	if err != nil {
		// return zero value for time if secret has not been created.
		if apierrors.IsNotFound(err) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	expirationTime, err := GetExpirationSecretAnnotation(secret)
	if err != nil {
		return time.Time{}, err
	}
	preRotationTime := expirationTime.Add(-r.preRotationWindow)
	return preRotationTime, nil
}

// Rotate implements the retrieval and storage of AWS sts credentials.
//
// This implements [Rotator.Rotate].
func (r *AWSOIDCRotator) Rotate(ctx context.Context, token string) error {
	r.logger.Info("rotating AWS sts temporary credentials",
		"namespace", r.backendSecurityPolicyNamespace,
		"name", r.backendSecurityPolicyName)

	result, err := r.assumeRoleWithToken(ctx, token)
	if err != nil {
		r.logger.Error(err, "failed to assume role", "role", r.roleArn, "access token", token)
		return err
	}

	secret, err := LookupSecret(ctx, r.client, r.backendSecurityPolicyNamespace, GetBSPSecretName(r.backendSecurityPolicyName))
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GetBSPSecretName(r.backendSecurityPolicyName),
				Namespace: r.backendSecurityPolicyNamespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: make(map[string][]byte),
		}
	}

	updateExpirationSecretAnnotation(secret, *result.Credentials.Expiration)

	// For now have profile as default.
	const defaultProfile = "default"
	credsFile := awsCredentialsFile{awsCredentials{
		profile:         defaultProfile,
		accessKeyID:     aws.ToString(result.Credentials.AccessKeyId),
		secretAccessKey: aws.ToString(result.Credentials.SecretAccessKey),
		sessionToken:    aws.ToString(result.Credentials.SessionToken),
		region:          r.region,
	}}

	updateAWSCredentialsInSecret(secret, &credsFile)

	err = r.client.Create(ctx, secret)
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return r.client.Update(ctx, secret)
		}
		return fmt.Errorf("failed to create secret: %w", err)
	}

	return nil
}

// assumeRoleWithToken exchanges an OIDC token for AWS credentials.
func (r *AWSOIDCRotator) assumeRoleWithToken(ctx context.Context, token string) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	return r.stsClient.AssumeRoleWithWebIdentity(ctx, &sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          aws.String(r.roleArn),
		WebIdentityToken: aws.String(token),
		RoleSessionName:  aws.String(fmt.Sprintf(awsSessionNameFormat, r.backendSecurityPolicyName)),
	})
}
