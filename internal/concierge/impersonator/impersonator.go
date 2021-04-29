// Copyright 2020-2021 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package impersonator

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/httpstream"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/sets"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/audit/policy"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/request/bearertoken"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/filterlatency"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/request"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/apiserver/pkg/server/filters"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	auditfake "k8s.io/apiserver/plugin/pkg/audit/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"

	"go.pinniped.dev/internal/constable"
	"go.pinniped.dev/internal/dynamiccert"
	"go.pinniped.dev/internal/httputil/securityheader"
	"go.pinniped.dev/internal/kubeclient"
	"go.pinniped.dev/internal/plog"
	"go.pinniped.dev/internal/valuelesscontext"
)

// FactoryFunc is a function which can create an impersonator server.
// It returns a function which will start the impersonator server.
// That start function takes a stopCh which can be used to stop the server.
// Once a server has been stopped, don't start it again using the start function.
// Instead, call the factory function again to get a new start function.
type FactoryFunc func(
	port int,
	dynamicCertProvider dynamiccert.Private,
	impersonationProxySignerCA dynamiccert.Public,
) (func(stopCh <-chan struct{}) error, error)

func New(
	port int,
	dynamicCertProvider dynamiccert.Private,
	impersonationProxySignerCA dynamiccert.Public,
) (func(stopCh <-chan struct{}) error, error) {
	return newInternal(port, dynamicCertProvider, impersonationProxySignerCA, nil, nil)
}

func newInternal( //nolint:funlen // yeah, it's kind of long.
	port int,
	dynamicCertProvider dynamiccert.Private,
	impersonationProxySignerCA dynamiccert.Public,
	clientOpts []kubeclient.Option, // for unit testing, should always be nil in production
	recOpts func(*genericoptions.RecommendedOptions), // for unit testing, should always be nil in production
) (func(stopCh <-chan struct{}) error, error) {
	var listener net.Listener

	constructServer := func() (func(stopCh <-chan struct{}) error, error) {
		// Bare minimum server side scheme to allow for status messages to be encoded.
		scheme := runtime.NewScheme()
		metav1.AddToGroupVersion(scheme, metav1.Unversioned)
		codecs := serializer.NewCodecFactory(scheme)

		// This is unused for now but it is a safe value that we could use in the future.
		defaultEtcdPathPrefix := "/pinniped-impersonation-proxy-registry"

		recommendedOptions := genericoptions.NewRecommendedOptions(
			defaultEtcdPathPrefix,
			codecs.LegacyCodec(),
		)
		recommendedOptions.Etcd = nil                                                   // turn off etcd storage because we don't need it yet
		recommendedOptions.SecureServing.ServerCert.GeneratedCert = dynamicCertProvider // serving certs (end user facing)
		recommendedOptions.SecureServing.BindPort = port

		// Wire up the impersonation proxy signer CA as another valid authenticator for client cert auth,
		// along with the Kube API server's CA.
		// Note: any changes to the the Authentication stack need to be kept in sync with any assumptions made
		// by getTransportForUser, especially if we ever update the TCR API to start returning bearer tokens.
		kubeClient, err := kubeclient.New(clientOpts...)
		if err != nil {
			return nil, err
		}
		kubeClientCA, err := dynamiccertificates.NewDynamicCAFromConfigMapController(
			"client-ca", metav1.NamespaceSystem, "extension-apiserver-authentication", "client-ca-file", kubeClient.Kubernetes,
		)
		if err != nil {
			return nil, err
		}
		recommendedOptions.Authentication.ClientCert.CAContentProvider = dynamiccertificates.NewUnionCAContentProvider(
			impersonationProxySignerCA, kubeClientCA,
		)

		if recOpts != nil {
			recOpts(recommendedOptions)
		}

		serverConfig := genericapiserver.NewRecommendedConfig(codecs)

		// Note that ApplyTo is going to create a network listener and bind to the requested port.
		// It puts this listener into serverConfig.SecureServing.Listener.
		err = recommendedOptions.ApplyTo(serverConfig)
		if serverConfig.SecureServing != nil {
			// Set the pointer from the outer function to allow the outer function to close the listener in case
			// this function returns an error for any reason anywhere below here.
			listener = serverConfig.SecureServing.Listener
		}
		if err != nil {
			return nil, err
		}

		// Loopback authentication to this server does not really make sense since we just proxy everything to
		// the Kube API server, thus we replace loopback connection config with one that does direct connections
		// the Kube API server. Loopback config is mainly used by post start hooks, so this is mostly future proofing.
		serverConfig.LoopbackClientConfig = rest.CopyConfig(kubeClient.ProtoConfig) // assume proto is safe (hooks can override)
		// Remove the bearer token so our authorizer does not get stomped on by AuthorizeClientBearerToken.
		// See sanity checks at the end of this function.
		serverConfig.LoopbackClientConfig.BearerToken = ""

		// match KAS exactly since our long running operations are just a proxy to it
		// this must be kept in sync with github.com/kubernetes/kubernetes/cmd/kube-apiserver/app/server.go
		// this is nothing to stress about - it has not changed since the beginning of Kube:
		// v1.6 no-op move away from regex to request info https://github.com/kubernetes/kubernetes/pull/38119
		// v1.1 added pods/attach to the list https://github.com/kubernetes/kubernetes/pull/13705
		serverConfig.LongRunningFunc = filters.BasicLongRunningRequestCheck(
			sets.NewString("watch", "proxy"),
			sets.NewString("attach", "exec", "proxy", "log", "portforward"),
		)

		// Assume proto config is safe because transport level configs do not use rest.ContentConfig.
		// Thus if we are interacting with actual APIs, they should be using pre-built clients.
		impersonationProxyFunc, err := newImpersonationReverseProxyFunc(rest.CopyConfig(kubeClient.ProtoConfig))
		if err != nil {
			return nil, err
		}

		defaultBuildHandlerChainFunc := serverConfig.BuildHandlerChainFunc
		serverConfig.BuildHandlerChainFunc = func(_ http.Handler, c *genericapiserver.Config) http.Handler {
			// We ignore the passed in handler because we never have any REST APIs to delegate to.
			// This means we are ignoring the admission, discovery, REST storage, etc layers.
			doNotDelegate := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})

			// Impersonation proxy business logic with timing information.
			impersonationProxyCompleted := filterlatency.TrackCompleted(doNotDelegate)
			impersonationProxy := impersonationProxyFunc(c)
			handler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer impersonationProxyCompleted.ServeHTTP(w, r)
				impersonationProxy.ServeHTTP(w, r)
			}))
			handler = filterlatency.TrackStarted(handler, "impersonationproxy")

			handler = filterlatency.TrackCompleted(handler)
			handler = deleteKnownImpersonationHeaders(handler)
			handler = filterlatency.TrackStarted(handler, "deleteimpersonationheaders")

			// The standard Kube handler chain (authn, authz, impersonation, audit, etc).
			// See the genericapiserver.DefaultBuildHandlerChain func for details.
			handler = defaultBuildHandlerChainFunc(handler, c)

			// we need to grab the bearer token before WithAuthentication deletes it.
			handler = filterlatency.TrackCompleted(handler)
			handler = withBearerTokenPreservation(handler)
			handler = filterlatency.TrackStarted(handler, "bearertokenpreservation")

			// Always set security headers so browsers do the right thing.
			handler = filterlatency.TrackCompleted(handler)
			handler = securityheader.Wrap(handler)
			handler = filterlatency.TrackStarted(handler, "securityheaders")

			return handler
		}

		// wire up a fake audit backend at the metadata level so we can preserve the original user during nested impersonation
		// TODO: wire up the real std out logging audit backend based on plog log level
		serverConfig.AuditPolicyChecker = policy.FakeChecker(auditinternal.LevelMetadata, nil)
		serverConfig.AuditBackend = &auditfake.Backend{}

		// if we ever start unioning a TCR bearer token authenticator with serverConfig.Authenticator
		// then we will need to update the related assumption in tokenPassthroughRoundTripper

		delegatingAuthorizer := serverConfig.Authorization.Authorizer
		nestedImpersonationAuthorizer := &comparableAuthorizer{
			authorizerFunc: func(ctx context.Context, a authorizer.Attributes) (authorizer.Decision, string, error) {
				switch a.GetVerb() {
				case "":
					// Empty string is disallowed because request info has had bugs in the past where it would leave it empty.
					return authorizer.DecisionDeny, "invalid verb", nil
				case "create",
					"update",
					"delete",
					"deletecollection",
					"get",
					"list",
					"watch",
					"patch",
					"proxy":
					// we know these verbs are from the request info parsing which is safe to delegate to KAS
					return authorizer.DecisionAllow, "deferring standard verb authorization to kube API server", nil
				default:
					// assume everything else is internal SAR checks that we need to run against the requesting user
					// because when KAS does the check, it may run the check against our service account and not the
					// requesting user.  This also handles the impersonate verb to allow for nested impersonation.
					return delegatingAuthorizer.Authorize(ctx, a)
				}
			},
		}
		// Set our custom authorizer before calling Compete(), which will use it.
		serverConfig.Authorization.Authorizer = nestedImpersonationAuthorizer

		impersonationProxyServer, err := serverConfig.Complete().New("impersonation-proxy", genericapiserver.NewEmptyDelegate())
		if err != nil {
			return nil, err
		}

		preparedRun := impersonationProxyServer.PrepareRun()

		// Sanity check. Make sure that our custom authorizer is still in place and did not get changed or wrapped.
		if preparedRun.Authorizer != nestedImpersonationAuthorizer {
			return nil, constable.Error("invalid mutation of impersonation authorizer detected")
		}

		// Sanity check. Assert that we have a functioning token file to use and no bearer token.
		if len(preparedRun.LoopbackClientConfig.BearerToken) != 0 || len(preparedRun.LoopbackClientConfig.BearerTokenFile) == 0 {
			return nil, constable.Error("invalid impersonator loopback rest config has wrong bearer token semantics")
		}

		return preparedRun.Run, nil
	}

	result, err := constructServer()
	// If there was any error during construction, then we would like to close the listener to free up the port.
	if err != nil {
		errs := []error{err}
		if listener != nil {
			errs = append(errs, listener.Close())
		}
		return nil, errors.NewAggregate(errs)
	}
	return result, nil
}

func deleteKnownImpersonationHeaders(delegate http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// remove known impersonation headers while avoiding mutation of input request
		// unknown future impersonation headers will still get caught by our later checks
		if ensureNoImpersonationHeaders(r) != nil {
			r = r.Clone(r.Context())

			impersonationHeaders := []string{
				transport.ImpersonateUserHeader,
				transport.ImpersonateGroupHeader,
			}

			for k := range r.Header {
				if !strings.HasPrefix(k, transport.ImpersonateUserExtraHeaderPrefix) {
					continue
				}
				impersonationHeaders = append(impersonationHeaders, k)
			}

			for _, header := range impersonationHeaders {
				r.Header.Del(header) // delay mutation until the end when we are done iterating over the map
			}
		}

		delegate.ServeHTTP(w, r)
	})
}

// No-op wrapping around AuthorizerFunc to allow for comparisons.
type comparableAuthorizer struct {
	authorizerFunc
}

// TODO: delete when we pick up https://github.com/kubernetes/kubernetes/pull/100963
type authorizerFunc func(ctx context.Context, a authorizer.Attributes) (authorizer.Decision, string, error)

func (f authorizerFunc) Authorize(ctx context.Context, a authorizer.Attributes) (authorizer.Decision, string, error) {
	return f(ctx, a)
}

func withBearerTokenPreservation(delegate http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// this looks a bit hacky but lets us avoid writing any logic for parsing out the bearer token
		var reqToken string
		_, _, _ = bearertoken.New(authenticator.TokenFunc(func(_ context.Context, token string) (*authenticator.Response, bool, error) {
			reqToken = token
			return nil, false, nil
		})).AuthenticateRequest(r)

		// smuggle the token through the context.  this does mean that we need to avoid logging the context.
		if len(reqToken) != 0 {
			ctx := context.WithValue(r.Context(), tokenKey, reqToken)
			r = r.WithContext(ctx)
		}

		delegate.ServeHTTP(w, r)
	})
}

func tokenFrom(ctx context.Context) string {
	token, _ := ctx.Value(tokenKey).(string)
	return token
}

// contextKey type is unexported to prevent collisions.
type contextKey int

const tokenKey contextKey = iota

func newImpersonationReverseProxyFunc(restConfig *rest.Config) (func(*genericapiserver.Config) http.Handler, error) {
	serverURL, err := url.Parse(restConfig.Host)
	if err != nil {
		return nil, fmt.Errorf("could not parse host URL from in-cluster config: %w", err)
	}

	http1RoundTripper, err := getTransportForProtocol(restConfig, "http/1.1")
	if err != nil {
		return nil, fmt.Errorf("could not get http/1.1 round tripper: %w", err)
	}
	http1RoundTripperAnonymous, err := getTransportForProtocol(rest.AnonymousClientConfig(restConfig), "http/1.1")
	if err != nil {
		return nil, fmt.Errorf("could not get http/1.1 anonymous round tripper: %w", err)
	}

	http2RoundTripper, err := getTransportForProtocol(restConfig, "h2")
	if err != nil {
		return nil, fmt.Errorf("could not get http/2.0 round tripper: %w", err)
	}
	http2RoundTripperAnonymous, err := getTransportForProtocol(rest.AnonymousClientConfig(restConfig), "h2")
	if err != nil {
		return nil, fmt.Errorf("could not get http/2.0 anonymous round tripper: %w", err)
	}

	return func(c *genericapiserver.Config) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.Header.Values("Authorization")) != 0 {
				plog.Warning("aggregated API server logic did not delete authorization header but it is always supposed to do so",
					"url", r.URL.String(),
					"method", r.Method,
				)
				newInternalErrResponse(w, r, c.Serializer, "invalid authorization header")
				return
			}

			if err := ensureNoImpersonationHeaders(r); err != nil {
				plog.Error("unknown impersonation header seen",
					err,
					"url", r.URL.String(),
					"method", r.Method,
				)
				newInternalErrResponse(w, r, c.Serializer, "invalid impersonation")
				return
			}

			userInfo, ok := request.UserFrom(r.Context())
			if !ok {
				plog.Warning("aggregated API server logic did not set user info but it is always supposed to do so",
					"url", r.URL.String(),
					"method", r.Method,
				)
				newInternalErrResponse(w, r, c.Serializer, "invalid user")
				return
			}

			ae := request.AuditEventFrom(r.Context())
			if ae == nil {
				plog.Warning("aggregated API server logic did not set audit event but it is always supposed to do so",
					"url", r.URL.String(),
					"method", r.Method,
				)
				newInternalErrResponse(w, r, c.Serializer, "invalid audit event")
				return
			}

			// grab the request's bearer token if present.  this is optional and does not fail the request if missing.
			token := tokenFrom(r.Context())

			// KAS only supports upgrades via http/1.1 to websockets/SPDY (upgrades never use http/2.0)
			// Thus we default to using http/2.0 when the request is not an upgrade, otherwise we use http/1.1
			baseRT, baseRTAnonymous := http2RoundTripper, http2RoundTripperAnonymous
			isUpgradeRequest := httpstream.IsUpgradeRequest(r)
			if isUpgradeRequest {
				baseRT, baseRTAnonymous = http1RoundTripper, http1RoundTripperAnonymous
			}

			rt, err := getTransportForUser(r.Context(), userInfo, baseRT, baseRTAnonymous, ae, token, c.Authentication.Authenticator)
			if err != nil {
				plog.WarningErr("rejecting request as we cannot act as the current user", err,
					"url", r.URL.String(),
					"method", r.Method,
					"isUpgradeRequest", isUpgradeRequest,
				)
				newInternalErrResponse(w, r, c.Serializer, "unimplemented functionality - unable to act as current user")
				return
			}

			plog.Debug("impersonation proxy servicing request",
				"url", r.URL.String(),
				"method", r.Method,
				"isUpgradeRequest", isUpgradeRequest,
			)
			plog.Trace("impersonation proxy servicing request was for user",
				"url", r.URL.String(),
				"method", r.Method,
				"isUpgradeRequest", isUpgradeRequest,
				"username", userInfo.GetName(), // this info leak seems fine for trace level logs
			)

			// The proxy library used below will panic when the client disconnects abruptly, so in order to
			// assure that this log message is always printed at the end of this func, it must be deferred.
			defer plog.Debug("impersonation proxy finished servicing request",
				"url", r.URL.String(),
				"method", r.Method,
				"isUpgradeRequest", isUpgradeRequest,
			)

			// do not allow the client to cause log confusion by spoofing this header
			if len(r.Header.Values("X-Forwarded-For")) > 0 {
				r = utilnet.CloneRequest(r)
				r.Header.Del("X-Forwarded-For")
			}

			reverseProxy := httputil.NewSingleHostReverseProxy(serverURL)
			reverseProxy.Transport = rt
			reverseProxy.FlushInterval = 200 * time.Millisecond // the "watch" verb will not work without this line
			reverseProxy.ServeHTTP(w, r)
		})
	}, nil
}

func ensureNoImpersonationHeaders(r *http.Request) error {
	for key := range r.Header {
		// even though we have unit tests that try to cover this case, it is hard to tell if Go does
		// client side canonicalization on encode, server side canonicalization on decode, or both
		key := http.CanonicalHeaderKey(key)
		if strings.HasPrefix(key, "Impersonate") {
			return fmt.Errorf("%q header already exists", key)
		}
	}

	return nil
}

func getTransportForUser(ctx context.Context, userInfo user.Info, delegate, delegateAnonymous http.RoundTripper, ae *auditinternal.Event, token string, authenticator authenticator.Request) (http.RoundTripper, error) {
	if canImpersonateFully(userInfo) {
		return standardImpersonationRoundTripper(userInfo, ae, delegate)
	}

	return tokenPassthroughRoundTripper(ctx, delegateAnonymous, ae, token, authenticator)
}

func canImpersonateFully(userInfo user.Info) bool {
	// nolint: gosimple  // this structure is on purpose because we plan to expand this function
	if len(userInfo.GetUID()) == 0 {
		return true
	}

	// once kube supports UID impersonation, add logic to detect if the KAS is
	// new enough to have this functionality and return true in that case as well
	return false
}

func standardImpersonationRoundTripper(userInfo user.Info, ae *auditinternal.Event, delegate http.RoundTripper) (http.RoundTripper, error) {
	extra, err := buildExtra(userInfo.GetExtra(), ae)
	if err != nil {
		return nil, err
	}

	impersonateConfig := transport.ImpersonationConfig{
		UserName: userInfo.GetName(),
		Groups:   userInfo.GetGroups(),
		Extra:    extra,
	}
	// transport.NewImpersonatingRoundTripper clones the request before setting headers
	// thus it will not accidentally mutate the input request (see http.Handler docs)
	return transport.NewImpersonatingRoundTripper(impersonateConfig, delegate), nil
}

func tokenPassthroughRoundTripper(ctx context.Context, delegateAnonymous http.RoundTripper, ae *auditinternal.Event, token string, authenticator authenticator.Request) (http.RoundTripper, error) {
	// all code below assumes KAS does not support UID impersonation because that case is handled in the standard path

	// it also assumes that the TCR API does not issue tokens - if this assumption changes, we will need
	// some way to distinguish a token that is only valid against this impersonation proxy and not against KAS.
	// this code will fail closed because said TCR token would not work against KAS and the request would fail.

	// if we get here we know the final user info had a UID
	// if the original user is also performing a nested impersonation, it means that said nested
	// impersonation is trying to impersonate a UID since final user info == ae.ImpersonatedUser
	// we know this KAS does not support UID impersonation so this request must be rejected
	if ae.ImpersonatedUser != nil {
		return nil, constable.Error("unable to impersonate uid")
	}

	// see what KAS thinks this token translates into
	// this is important because certs have precedence over tokens and we want
	// to make sure that we do not get confused and pass along the wrong token
	tokenUser, err := tokenReview(ctx, token, authenticator)
	if err != nil {
		return nil, err
	}

	// we want to compare the result of the token authentication with the original user that made the request
	// if the user who made the request and the token do not match, we cannot go any further at this point
	if !apiequality.Semantic.DeepEqual(ae.User, tokenUser) {
		// this info leak seems fine for trace level logs
		plog.Trace("failed to passthrough token due to user mismatch",
			"original-username", ae.User.Username,
			"original-uid", ae.User.UID,
			"token-username", tokenUser.Username,
			"token-uid", tokenUser.UID,
		)
		return nil, constable.Error("token authenticated as a different user")
	}

	// now we know that if we send this token to KAS, it will authenticate correctly
	return transport.NewBearerAuthRoundTripper(token, delegateAnonymous), nil
}

func tokenReview(ctx context.Context, token string, authenticator authenticator.Request) (authenticationv1.UserInfo, error) {
	if len(token) == 0 {
		return authenticationv1.UserInfo{}, constable.Error("no token on request")
	}

	// create a header that contains nothing but the token
	// an astute observer may ask "but what about the token's audience?"
	// in this case, we want to leave audiences unset per the token review docs:
	// > If no audiences are provided, the audience will default to the audience of the Kubernetes apiserver.
	// i.e. we want to make sure that the given token is valid against KAS
	fakeReq := &http.Request{Header: http.Header{}}
	fakeReq.Header.Set("Authorization", "Bearer "+token)

	// propagate cancellation of parent context (without any values such as audience)
	fakeReq = fakeReq.WithContext(valuelesscontext.New(ctx))

	// this will almost always be a free call that hits our 10 second cache TTL
	resp, ok, err := authenticator.AuthenticateRequest(fakeReq)
	if err != nil {
		return authenticationv1.UserInfo{}, err
	}
	if !ok {
		return authenticationv1.UserInfo{}, constable.Error("token failed to authenticate")
	}

	tokenUser := authenticationv1.UserInfo{
		Username: resp.User.GetName(),
		UID:      resp.User.GetUID(),
		Groups:   resp.User.GetGroups(),
		Extra:    make(map[string]authenticationv1.ExtraValue, len(resp.User.GetExtra())),
	}
	for k, v := range resp.User.GetExtra() {
		tokenUser.Extra[k] = v
	}

	return tokenUser, nil
}

func buildExtra(extra map[string][]string, ae *auditinternal.Event) (map[string][]string, error) {
	const reservedImpersonationProxySuffix = ".impersonation-proxy.concierge.pinniped.dev"

	// always validate that the extra is something we support irregardless of nested impersonation
	for k := range extra {
		if !extraKeyRegexp.MatchString(k) {
			return nil, fmt.Errorf("disallowed extra key seen: %s", k)
		}

		if strings.HasSuffix(k, reservedImpersonationProxySuffix) {
			return nil, fmt.Errorf("disallowed extra key with reserved prefix seen: %s", k)
		}
	}

	if ae.ImpersonatedUser == nil {
		return extra, nil // just return the given extra since nested impersonation is not being used
	}

	// avoid mutating input map, preallocate new map to store original user info
	out := make(map[string][]string, len(extra)+1)

	for k, v := range extra {
		out[k] = v // shallow copy of slice since we are not going to mutate it
	}

	origUserInfoJSON, err := json.Marshal(ae.User)
	if err != nil {
		return nil, err
	}

	out["original-user-info"+reservedImpersonationProxySuffix] = []string{string(origUserInfoJSON)}

	return out, nil
}

// extraKeyRegexp is a very conservative regex to handle impersonation's extra key fidelity limitations such as casing and escaping.
var extraKeyRegexp = regexp.MustCompile(`^[a-z0-9/\-._]+$`)

func newInternalErrResponse(w http.ResponseWriter, r *http.Request, s runtime.NegotiatedSerializer, msg string) {
	newStatusErrResponse(w, r, s, apierrors.NewInternalError(constable.Error(msg)))
}

func newStatusErrResponse(w http.ResponseWriter, r *http.Request, s runtime.NegotiatedSerializer, err *apierrors.StatusError) {
	requestInfo, ok := genericapirequest.RequestInfoFrom(r.Context())
	if !ok {
		responsewriters.InternalError(w, r, constable.Error("no RequestInfo found in the context"))
		return
	}

	gv := schema.GroupVersion{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion}
	responsewriters.ErrorNegotiated(err, s, gv, w, r)
}

func getTransportForProtocol(restConfig *rest.Config, protocol string) (http.RoundTripper, error) {
	transportConfig, err := restConfig.TransportConfig()
	if err != nil {
		return nil, fmt.Errorf("could not get in-cluster transport config: %w", err)
	}
	transportConfig.TLS.NextProtos = []string{protocol}

	return transport.New(transportConfig)
}
