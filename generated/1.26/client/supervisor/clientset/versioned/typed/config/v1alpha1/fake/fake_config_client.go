// Copyright 2020-2023 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	v1alpha1 "go.pinniped.dev/generated/1.26/client/supervisor/clientset/versioned/typed/config/v1alpha1"
	rest "k8s.io/client-go/rest"
	testing "k8s.io/client-go/testing"
)

type FakeConfigV1alpha1 struct {
	*testing.Fake
}

func (c *FakeConfigV1alpha1) FederationDomains(namespace string) v1alpha1.FederationDomainInterface {
	return &FakeFederationDomains{c, namespace}
}

func (c *FakeConfigV1alpha1) OIDCClients(namespace string) v1alpha1.OIDCClientInterface {
	return &FakeOIDCClients{c, namespace}
}

// RESTClient returns a RESTClient that is used to communicate
// with API server by this client implementation.
func (c *FakeConfigV1alpha1) RESTClient() rest.Interface {
	var ret *rest.RESTClient
	return ret
}
