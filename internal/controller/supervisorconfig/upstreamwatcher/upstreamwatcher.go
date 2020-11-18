// Copyright 2020 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package upstreamwatcher implements a controller that watches UpstreamOIDCProvider objects.
package upstreamwatcher

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/coreos/go-oidc"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/cache"
	corev1informers "k8s.io/client-go/informers/core/v1"

	"go.pinniped.dev/generated/1.19/apis/supervisor/idp/v1alpha1"
	pinnipedclientset "go.pinniped.dev/generated/1.19/client/supervisor/clientset/versioned"
	idpinformers "go.pinniped.dev/generated/1.19/client/supervisor/informers/externalversions/idp/v1alpha1"
	"go.pinniped.dev/internal/constable"
	pinnipedcontroller "go.pinniped.dev/internal/controller"
	"go.pinniped.dev/internal/controllerlib"
	"go.pinniped.dev/internal/oidc/provider"
)

const (
	// Setup for the name of our controller in logs.
	controllerName = "upstream-observer"

	// Constants related to the client credentials Secret.
	oidcClientSecretType = "secrets.pinniped.dev/oidc-client"
	clientIDDataKey      = "clientID"
	clientSecretDataKey  = "clientSecret"

	// Constants related to the OIDC provider discovery cache. These do not affect the cache of JWKS.
	validatorCacheTTL = 15 * time.Minute

	// Constants related to conditions.
	typeClientCredsValid       = "ClientCredentialsValid"
	typeOIDCDiscoverySucceeded = "OIDCDiscoverySucceeded"
	reasonNotFound             = "SecretNotFound"
	reasonWrongType            = "SecretWrongType"
	reasonMissingKeys          = "SecretMissingKeys"
	reasonSuccess              = "Success"
	reasonUnreachable          = "Unreachable"
	reasonInvalidTLSConfig     = "InvalidTLSConfig"
	reasonInvalidResponse      = "InvalidResponse"

	// Errors that are generated by our reconcile process.
	errFailureStatus  = constable.Error("UpstreamOIDCProvider has a failing condition")
	errNoCertificates = constable.Error("no certificates found")
)

// IDPCache is a thread safe cache that holds a list of validated upstream OIDC IDP configurations.
type IDPCache interface {
	SetIDPList([]provider.UpstreamOIDCIdentityProviderI)
}

// lruValidatorCache caches the *oidc.Provider associated with a particular issuer/TLS configuration.
type lruValidatorCache struct{ cache *cache.Expiring }

func (c *lruValidatorCache) getProvider(spec *v1alpha1.UpstreamOIDCProviderSpec) *oidc.Provider {
	if result, ok := c.cache.Get(c.cacheKey(spec)); ok {
		return result.(*oidc.Provider)
	}
	return nil
}

func (c *lruValidatorCache) putProvider(spec *v1alpha1.UpstreamOIDCProviderSpec, provider *oidc.Provider) {
	c.cache.Set(c.cacheKey(spec), provider, validatorCacheTTL)
}

func (c *lruValidatorCache) cacheKey(spec *v1alpha1.UpstreamOIDCProviderSpec) interface{} {
	var key struct{ issuer, caBundle string }
	key.issuer = spec.Issuer
	if spec.TLS != nil {
		key.caBundle = spec.TLS.CertificateAuthorityData
	}
	return key
}

type controller struct {
	cache          IDPCache
	log            logr.Logger
	client         pinnipedclientset.Interface
	providers      idpinformers.UpstreamOIDCProviderInformer
	secrets        corev1informers.SecretInformer
	validatorCache interface {
		getProvider(spec *v1alpha1.UpstreamOIDCProviderSpec) *oidc.Provider
		putProvider(spec *v1alpha1.UpstreamOIDCProviderSpec, provider *oidc.Provider)
	}
}

// New instantiates a new controllerlib.Controller which will populate the provided IDPCache.
func New(
	idpCache IDPCache,
	client pinnipedclientset.Interface,
	providers idpinformers.UpstreamOIDCProviderInformer,
	secrets corev1informers.SecretInformer,
	log logr.Logger,
) controllerlib.Controller {
	c := controller{
		cache:          idpCache,
		log:            log.WithName(controllerName),
		client:         client,
		providers:      providers,
		secrets:        secrets,
		validatorCache: &lruValidatorCache{cache: cache.NewExpiring()},
	}
	filter := pinnipedcontroller.MatchAnythingFilter(pinnipedcontroller.SingletonQueue())
	return controllerlib.New(
		controllerlib.Config{Name: controllerName, Syncer: &c},
		controllerlib.WithInformer(providers, filter, controllerlib.InformerOption{}),
		controllerlib.WithInformer(secrets, filter, controllerlib.InformerOption{}),
	)
}

// Sync implements controllerlib.Syncer.
func (c *controller) Sync(ctx controllerlib.Context) error {
	actualUpstreams, err := c.providers.Lister().List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list UpstreamOIDCProviders: %w", err)
	}

	requeue := false
	validatedUpstreams := make([]provider.UpstreamOIDCIdentityProviderI, 0, len(actualUpstreams))
	for _, upstream := range actualUpstreams {
		valid := c.validateUpstream(ctx, upstream)
		if valid == nil {
			requeue = true
		} else {
			validatedUpstreams = append(validatedUpstreams, provider.UpstreamOIDCIdentityProviderI(valid))
		}
	}
	c.cache.SetIDPList(validatedUpstreams)
	if requeue {
		return controllerlib.ErrSyntheticRequeue
	}
	return nil
}

// validateUpstream validates the provided v1alpha1.UpstreamOIDCProvider and returns the validated configuration as a
// provider.UpstreamOIDCIdentityProvider. As a side effect, it also updates the status of the v1alpha1.UpstreamOIDCProvider.
func (c *controller) validateUpstream(ctx controllerlib.Context, upstream *v1alpha1.UpstreamOIDCProvider) *provider.UpstreamOIDCIdentityProvider {
	result := provider.UpstreamOIDCIdentityProvider{
		Name:   upstream.Name,
		Scopes: computeScopes(upstream.Spec.AuthorizationConfig.AdditionalScopes),
	}
	conditions := []*v1alpha1.Condition{
		c.validateSecret(upstream, &result),
		c.validateIssuer(ctx.Context, upstream, &result),
	}
	c.updateStatus(ctx.Context, upstream, conditions)

	valid := true
	log := c.log.WithValues("namespace", upstream.Namespace, "name", upstream.Name)
	for _, condition := range conditions {
		if condition.Status == v1alpha1.ConditionFalse {
			valid = false
			log.WithValues(
				"type", condition.Type,
				"reason", condition.Reason,
				"message", condition.Message,
			).Error(errFailureStatus, "found failing condition")
		}
	}
	if valid {
		return &result
	}
	return nil
}

// validateSecret validates the .spec.client.secretName field and returns the appropriate ClientCredentialsValid condition.
func (c *controller) validateSecret(upstream *v1alpha1.UpstreamOIDCProvider, result *provider.UpstreamOIDCIdentityProvider) *v1alpha1.Condition {
	secretName := upstream.Spec.Client.SecretName

	// Fetch the Secret from informer cache.
	secret, err := c.secrets.Lister().Secrets(upstream.Namespace).Get(secretName)
	if err != nil {
		return &v1alpha1.Condition{
			Type:    typeClientCredsValid,
			Status:  v1alpha1.ConditionFalse,
			Reason:  reasonNotFound,
			Message: err.Error(),
		}
	}

	// Validate the secret .type field.
	if secret.Type != oidcClientSecretType {
		return &v1alpha1.Condition{
			Type:    typeClientCredsValid,
			Status:  v1alpha1.ConditionFalse,
			Reason:  reasonWrongType,
			Message: fmt.Sprintf("referenced Secret %q has wrong type %q (should be %q)", secretName, secret.Type, oidcClientSecretType),
		}
	}

	// Validate the secret .data field.
	clientID := secret.Data[clientIDDataKey]
	clientSecret := secret.Data[clientSecretDataKey]
	if len(clientID) == 0 || len(clientSecret) == 0 {
		return &v1alpha1.Condition{
			Type:    typeClientCredsValid,
			Status:  v1alpha1.ConditionFalse,
			Reason:  reasonMissingKeys,
			Message: fmt.Sprintf("referenced Secret %q is missing required keys %q", secretName, []string{clientIDDataKey, clientSecretDataKey}),
		}
	}

	// If everything is valid, update the result and set the condition to true.
	result.ClientID = string(clientID)
	return &v1alpha1.Condition{
		Type:    typeClientCredsValid,
		Status:  v1alpha1.ConditionTrue,
		Reason:  reasonSuccess,
		Message: "loaded client credentials",
	}
}

// validateIssuer validates the .spec.issuer field, performs OIDC discovery, and returns the appropriate OIDCDiscoverySucceeded condition.
func (c *controller) validateIssuer(ctx context.Context, upstream *v1alpha1.UpstreamOIDCProvider, result *provider.UpstreamOIDCIdentityProvider) *v1alpha1.Condition {
	// Get the provider (from cache if possible).
	discoveredProvider := c.validatorCache.getProvider(&upstream.Spec)

	// If the provider does not exist in the cache, do a fresh discovery lookup and save to the cache.
	if discoveredProvider == nil {
		tlsConfig, err := getTLSConfig(upstream)
		if err != nil {
			return &v1alpha1.Condition{
				Type:    typeOIDCDiscoverySucceeded,
				Status:  v1alpha1.ConditionFalse,
				Reason:  reasonInvalidTLSConfig,
				Message: err.Error(),
			}
		}
		httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}

		discoveredProvider, err = oidc.NewProvider(oidc.ClientContext(ctx, httpClient), upstream.Spec.Issuer)
		if err != nil {
			return &v1alpha1.Condition{
				Type:    typeOIDCDiscoverySucceeded,
				Status:  v1alpha1.ConditionFalse,
				Reason:  reasonUnreachable,
				Message: fmt.Sprintf("failed to perform OIDC discovery against %q", upstream.Spec.Issuer),
			}
		}

		// Update the cache with the newly discovered value.
		c.validatorCache.putProvider(&upstream.Spec, discoveredProvider)
	}

	// TODO also parse the token endpoint from the discovery info and put it onto the `result`

	// Parse out and validate the discovered authorize endpoint.
	authURL, err := url.Parse(discoveredProvider.Endpoint().AuthURL)
	if err != nil {
		return &v1alpha1.Condition{
			Type:    typeOIDCDiscoverySucceeded,
			Status:  v1alpha1.ConditionFalse,
			Reason:  reasonInvalidResponse,
			Message: fmt.Sprintf("failed to parse authorization endpoint URL: %v", err),
		}
	}
	if authURL.Scheme != "https" {
		return &v1alpha1.Condition{
			Type:    typeOIDCDiscoverySucceeded,
			Status:  v1alpha1.ConditionFalse,
			Reason:  reasonInvalidResponse,
			Message: fmt.Sprintf(`authorization endpoint URL scheme must be "https", not %q`, authURL.Scheme),
		}
	}

	// If everything is valid, update the result and set the condition to true.
	result.AuthorizationURL = *authURL
	return &v1alpha1.Condition{
		Type:    typeOIDCDiscoverySucceeded,
		Status:  v1alpha1.ConditionTrue,
		Reason:  reasonSuccess,
		Message: "discovered issuer configuration",
	}
}

func getTLSConfig(upstream *v1alpha1.UpstreamOIDCProvider) (*tls.Config, error) {
	result := tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	if upstream.Spec.TLS == nil || upstream.Spec.TLS.CertificateAuthorityData == "" {
		return &result, nil
	}

	bundle, err := base64.StdEncoding.DecodeString(upstream.Spec.TLS.CertificateAuthorityData)
	if err != nil {
		return nil, fmt.Errorf("spec.certificateAuthorityData is invalid: %w", err)
	}

	result.RootCAs = x509.NewCertPool()
	if !result.RootCAs.AppendCertsFromPEM(bundle) {
		return nil, fmt.Errorf("spec.certificateAuthorityData is invalid: %w", errNoCertificates)
	}

	return &result, nil
}

func (c *controller) updateStatus(ctx context.Context, upstream *v1alpha1.UpstreamOIDCProvider, conditions []*v1alpha1.Condition) {
	log := c.log.WithValues("namespace", upstream.Namespace, "name", upstream.Name)
	updated := upstream.DeepCopy()

	updated.Status.Phase = v1alpha1.PhaseReady

	for i := range conditions {
		cond := conditions[i].DeepCopy()
		cond.LastTransitionTime = metav1.Now()
		cond.ObservedGeneration = upstream.Generation
		if mergeCondition(&updated.Status.Conditions, cond) {
			log.Info("updated condition", "type", cond.Type, "status", cond.Status, "reason", cond.Reason, "message", cond.Message)
		}
		if cond.Status == v1alpha1.ConditionFalse {
			updated.Status.Phase = v1alpha1.PhaseError
		}
	}

	sort.SliceStable(updated.Status.Conditions, func(i, j int) bool {
		return updated.Status.Conditions[i].Type < updated.Status.Conditions[j].Type
	})

	if equality.Semantic.DeepEqual(upstream, updated) {
		return
	}

	_, err := c.client.
		IDPV1alpha1().
		UpstreamOIDCProviders(upstream.Namespace).
		UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		log.Error(err, "failed to update status")
	}
}

// mergeCondition merges a new v1alpha1.Condition into a slice of existing conditions. It returns true
// if the condition has meaningfully changed.
func mergeCondition(existing *[]v1alpha1.Condition, new *v1alpha1.Condition) bool {
	// Find any existing condition with a matching type.
	var old *v1alpha1.Condition
	for i := range *existing {
		if (*existing)[i].Type == new.Type {
			old = &(*existing)[i]
			continue
		}
	}

	// If there is no existing condition of this type, append this one and we're done.
	if old == nil {
		*existing = append(*existing, *new)
		return true
	}

	// Set the LastTransitionTime depending on whether the status has changed.
	new = new.DeepCopy()
	if old.Status == new.Status {
		new.LastTransitionTime = old.LastTransitionTime
	}

	// If anything has actually changed, update the entry and return true.
	if !equality.Semantic.DeepEqual(old, new) {
		*old = *new
		return true
	}

	// Otherwise the entry is already up to date.
	return false
}

func computeScopes(additionalScopes []string) []string {
	// First compute the unique set of scopes, including "openid" (de-duplicate).
	set := make(map[string]bool, len(additionalScopes)+1)
	set["openid"] = true
	for _, s := range additionalScopes {
		set[s] = true
	}

	// Then grab all the keys and sort them.
	scopes := make([]string, 0, len(set))
	for s := range set {
		scopes = append(scopes, s)
	}
	sort.Strings(scopes)
	return scopes
}
