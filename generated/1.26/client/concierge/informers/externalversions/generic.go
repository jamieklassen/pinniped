// Copyright 2020-2023 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Code generated by informer-gen. DO NOT EDIT.

package externalversions

import (
	"fmt"

	v1alpha1 "go.pinniped.dev/generated/1.26/apis/concierge/authentication/v1alpha1"
	configv1alpha1 "go.pinniped.dev/generated/1.26/apis/concierge/config/v1alpha1"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	cache "k8s.io/client-go/tools/cache"
)

// GenericInformer is type of SharedIndexInformer which will locate and delegate to other
// sharedInformers based on type
type GenericInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() cache.GenericLister
}

type genericInformer struct {
	informer cache.SharedIndexInformer
	resource schema.GroupResource
}

// Informer returns the SharedIndexInformer.
func (f *genericInformer) Informer() cache.SharedIndexInformer {
	return f.informer
}

// Lister returns the GenericLister.
func (f *genericInformer) Lister() cache.GenericLister {
	return cache.NewGenericLister(f.Informer().GetIndexer(), f.resource)
}

// ForResource gives generic access to a shared informer of the matching type
// TODO extend this to unknown resources with a client pool
func (f *sharedInformerFactory) ForResource(resource schema.GroupVersionResource) (GenericInformer, error) {
	switch resource {
	// Group=authentication.concierge.pinniped.dev, Version=v1alpha1
	case v1alpha1.SchemeGroupVersion.WithResource("jwtauthenticators"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Authentication().V1alpha1().JWTAuthenticators().Informer()}, nil
	case v1alpha1.SchemeGroupVersion.WithResource("webhookauthenticators"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Authentication().V1alpha1().WebhookAuthenticators().Informer()}, nil

		// Group=config.concierge.pinniped.dev, Version=v1alpha1
	case configv1alpha1.SchemeGroupVersion.WithResource("credentialissuers"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Config().V1alpha1().CredentialIssuers().Informer()}, nil

	}

	return nil, fmt.Errorf("no informer found for %v", resource)
}
