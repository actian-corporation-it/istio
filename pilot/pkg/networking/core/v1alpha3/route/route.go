// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package route

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	xdsfault "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/common/fault/v3"
	xdshttpfault "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/fault/v3"
	matcher "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	xdstype "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	wellknown "github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/any"
	"github.com/golang/protobuf/ptypes/duration"
	"github.com/golang/protobuf/ptypes/wrappers"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/core/v1alpha3/route/retry"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/util/gogo"
	"istio.io/pkg/log"
)

// Headers with special meaning in Envoy
const (
	HeaderMethod    = ":method"
	HeaderAuthority = ":authority"
	HeaderScheme    = ":scheme"
)

// DefaultRouteName is the name assigned to a route generated by default in absence of a virtual service.
const DefaultRouteName = "default"

var (
	regexEngine = &matcher.RegexMatcher_GoogleRe2{GoogleRe2: &matcher.RegexMatcher_GoogleRE2{}}
)

// VirtualHostWrapper is a context-dependent virtual host entry with guarded routes.
// Note: Currently we are not fully utilizing this structure. We could invoke this logic
// once for all sidecars in the cluster to compute all RDS for inside the mesh and arrange
// it by listener port. However to properly use such an optimization, we need to have an
// eventing subsystem to invalidate the computed routes if any service changes/virtual services change.
type VirtualHostWrapper struct {
	// Port is the listener port for outbound sidecar (e.g. service port)
	Port int

	// Services are the services from the registry. Each service
	// in this list should have a virtual host entry
	Services []*model.Service

	// VirtualServiceHosts is a list of hosts defined in the virtual service
	// if virtual service hostname is same as a the service registry host, then
	// the host would appear in Services as we need to generate all variants of the
	// service's hostname within a platform (e.g., foo, foo.default, foo.default.svc, etc.)
	VirtualServiceHosts []string

	// Routes in the virtual host
	Routes []*route.Route
}

// BuildSidecarVirtualHostsFromConfigAndRegistry creates virtual hosts from
// the given set of virtual services and a list of services from the
// service registry. Services are indexed by FQDN hostnames.
func BuildSidecarVirtualHostsFromConfigAndRegistry(
	node *model.Proxy,
	push *model.PushContext,
	serviceRegistry map[host.Name]*model.Service,
	virtualServices []model.Config,
	listenPort int) []VirtualHostWrapper {

	out := make([]VirtualHostWrapper, 0)

	// translate all virtual service configs into virtual hosts
	for _, virtualService := range virtualServices {
		wrappers := buildSidecarVirtualHostsForVirtualService(node, push, virtualService, serviceRegistry, listenPort)
		if len(wrappers) == 0 {
			// If none of the routes matched by source (i.e. proxyLabels), then discard this entire virtual service
			continue
		}
		out = append(out, wrappers...)
	}

	// compute services missing virtual service configs
	missing := make(map[host.Name]bool)
	for fqdn := range serviceRegistry {
		missing[fqdn] = true
	}
	for _, wrapper := range out {
		for _, service := range wrapper.Services {
			delete(missing, service.Hostname)
		}
	}

	// append default hosts for the service missing virtual services
	for fqdn := range missing {
		svc := serviceRegistry[fqdn]
		for _, port := range svc.Ports {
			if port.Protocol.IsHTTP() || util.IsProtocolSniffingEnabledForPort(port) {
				cluster := model.BuildSubsetKey(model.TrafficDirectionOutbound, "", svc.Hostname, port.Port)
				traceOperation := traceOperation(string(svc.Hostname), port.Port)
				httpRoute := BuildDefaultHTTPOutboundRoute(node, cluster, traceOperation)

				// if this host has no virtualservice, the consistentHash on its destinationRule will be useless
				if hashPolicy := getHashPolicyByService(node, push, svc, port); hashPolicy != nil {
					httpRoute.GetRoute().HashPolicy = []*route.RouteAction_HashPolicy{hashPolicy}
				}
				out = append(out, VirtualHostWrapper{
					Port:     port.Port,
					Services: []*model.Service{svc},
					Routes:   []*route.Route{httpRoute},
				})
			}
		}
	}

	return out
}

// separateVSHostsAndServices splits the virtual service hosts into services (if they are found in the registry) and
// plain non-registry hostnames
func separateVSHostsAndServices(virtualService model.Config,
	serviceRegistry map[host.Name]*model.Service) ([]string, []*model.Service) {
	rule := virtualService.Spec.(*networking.VirtualService)
	hosts := make([]string, 0)
	servicesInVirtualService := make([]*model.Service, 0)
	wchosts := make([]host.Name, 0)

	// As a performance optimization, process non wildcard hosts first, so that they can be
	// looked up directly in the service registry map.
	for _, hostname := range rule.Hosts {
		vshost := host.Name(hostname)
		if !vshost.IsWildCarded() {
			if svc, exists := serviceRegistry[vshost]; exists {
				servicesInVirtualService = append(servicesInVirtualService, svc)
			} else {
				hosts = append(hosts, hostname)
			}
		} else {
			// Add it to the wildcard hosts so that they can be processed later.
			wchosts = append(wchosts, vshost)
		}
	}

	// Now process wild card hosts as they need to follow the slow path of looping through all services in the registry.
	for _, hostname := range wchosts {
		// Say host is *.global
		foundSvcMatch := false
		// Say we have services *.foo.global, *.bar.global
		for svcHost, svc := range serviceRegistry {
			// *.foo.global matches *.global
			if svcHost.Matches(hostname) {
				servicesInVirtualService = append(servicesInVirtualService, svc)
				foundSvcMatch = true
			}
		}
		if !foundSvcMatch {
			hosts = append(hosts, string(hostname))
		}
	}
	return hosts, servicesInVirtualService
}

// buildSidecarVirtualHostsForVirtualService creates virtual hosts corresponding to a virtual service.
// Called for each port to determine the list of vhosts on the given port.
// It may return an empty list if no VirtualService rule has a matching service.
func buildSidecarVirtualHostsForVirtualService(
	node *model.Proxy,
	push *model.PushContext,
	virtualService model.Config,
	serviceRegistry map[host.Name]*model.Service,
	listenPort int) []VirtualHostWrapper {
	hosts, servicesInVirtualService := separateVSHostsAndServices(virtualService, serviceRegistry)

	// Now group these services by port so that we can infer the destination.port if the user
	// doesn't specify any port for a multiport service. We need to know the destination port in
	// order to build the cluster name (outbound|<port>|<subset>|<serviceFQDN>)
	// If the destination service is being accessed on port X, we set that as the default
	// destination port
	serviceByPort := make(map[int][]*model.Service)
	for _, svc := range servicesInVirtualService {
		for _, port := range svc.Ports {
			if port.Protocol.IsHTTP() || util.IsProtocolSniffingEnabledForPort(port) {
				serviceByPort[port.Port] = append(serviceByPort[port.Port], svc)
			}
		}
	}

	// We need to group the virtual hosts by port, because each http connection manager is
	// going to send a separate RDS request
	// Note that we need to build non-default HTTP routes only for the virtual services.
	// The services in the serviceRegistry will always have a default route (/)
	if len(serviceByPort) == 0 {
		// This is a gross HACK. Fix me. Its a much bigger surgery though, due to the way
		// the current code is written.
		serviceByPort[80] = nil
	}
	meshGateway := map[string]bool{constants.IstioMeshGateway: true}
	out := make([]VirtualHostWrapper, 0, len(serviceByPort))
	routes, err := BuildHTTPRoutesForVirtualService(node, push, virtualService, serviceRegistry, listenPort, meshGateway)
	if err != nil || len(routes) == 0 {
		return out
	}
	for port, portServices := range serviceByPort {
		out = append(out, VirtualHostWrapper{
			Port:                port,
			Services:            portServices,
			VirtualServiceHosts: hosts,
			Routes:              routes,
		})
	}

	return out
}

// GetDestinationCluster generates a cluster name for the route, or error if no cluster
// can be found. Called by translateRule to determine if
func GetDestinationCluster(destination *networking.Destination, service *model.Service, listenerPort int) string {
	port := listenerPort
	if destination.GetPort() != nil {
		port = int(destination.GetPort().GetNumber())
	} else if service != nil && len(service.Ports) == 1 {
		// if service only has one port defined, use that as the port, otherwise use default listenerPort
		port = service.Ports[0].Port

		// Do not return blackhole cluster for service==nil case as there is a legitimate use case for
		// calling this function with nil service: to route to a pre-defined statically configured cluster
		// declared as part of the bootstrap.
		// If blackhole cluster is needed, do the check on the caller side. See gateway and tls.go for examples.
	}

	return model.BuildSubsetKey(model.TrafficDirectionOutbound, destination.Subset, host.Name(destination.Host), port)
}

// BuildHTTPRoutesForVirtualService creates data plane HTTP routes from the virtual service spec.
// The rule should be adapted to destination names (outbound clusters).
// Each rule is guarded by source labels.
//
// This is called for each port to compute virtual hosts.
// Each VirtualService is tried, with a list of services that listen on the port.
// Error indicates the given virtualService can't be used on the port.
// This function is used by both the gateway and the sidecar
func BuildHTTPRoutesForVirtualService(
	node *model.Proxy,
	push *model.PushContext,
	virtualService model.Config,
	serviceRegistry map[host.Name]*model.Service,
	listenPort int,
	gatewayNames map[string]bool) ([]*route.Route, error) {

	vs, ok := virtualService.Spec.(*networking.VirtualService)
	if !ok { // should never happen
		return nil, fmt.Errorf("in not a virtual service: %#v", virtualService)
	}

	out := make([]*route.Route, 0, len(vs.Http))

	for _, http := range vs.Http {
		if len(http.Match) == 0 {
			if r := translateRoute(push, node, http, nil, listenPort, virtualService, serviceRegistry, gatewayNames); r != nil {
				out = append(out, r)
			}
			// We have a rule with catch all match prefix: /. Other rules are of no use.
			break
		} else {
			if match := catchAllMatch(http); match != nil {
				// We have a catch all match block in the route, check if it is valid - A catch all match block is not valid
				// (translateRoute returns nil), if source or port match fails.
				if r := translateRoute(push, node, http, match, listenPort, virtualService, serviceRegistry, gatewayNames); r != nil {
					// We have a valid catch all route. No point building other routes, with match conditions.
					out = append(out, r)
					break
				}
			}
			for _, match := range http.Match {
				if r := translateRoute(push, node, http, match, listenPort, virtualService, serviceRegistry, gatewayNames); r != nil {
					out = append(out, r)
				}
			}
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no routes matched")
	}
	return out, nil
}

// sourceMatchHttp checks if the sourceLabels or the gateways in a match condition match with the
// labels for the proxy or the gateway name for which we are generating a route
func sourceMatchHTTP(match *networking.HTTPMatchRequest, proxyLabels labels.Collection, gatewayNames map[string]bool, proxyNamespace string) bool {
	if match == nil {
		return true
	}

	// Trim by source labels or mesh gateway
	if len(match.Gateways) > 0 {
		for _, g := range match.Gateways {
			if gatewayNames[g] {
				return true
			}
		}
	} else if proxyLabels.IsSupersetOf(match.GetSourceLabels()) {
		return match.SourceNamespace == "" || match.SourceNamespace == proxyNamespace
	}

	return false
}

// translateRoute translates HTTP routes
func translateRoute(push *model.PushContext, node *model.Proxy, in *networking.HTTPRoute,
	match *networking.HTTPMatchRequest, port int,
	virtualService model.Config,
	serviceRegistry map[host.Name]*model.Service,
	gatewayNames map[string]bool) *route.Route {

	// When building routes, its okay if the target cluster cannot be
	// resolved Traffic to such clusters will blackhole.

	// Match by source labels/gateway names inside the match condition
	if !sourceMatchHTTP(match, labels.Collection{node.Metadata.Labels}, gatewayNames, node.Metadata.Namespace) {
		return nil
	}

	// Match by the destination port specified in the match condition
	if match != nil && match.Port != 0 && match.Port != uint32(port) {
		return nil
	}

	out := &route.Route{
		Match:    translateRouteMatch(match, node),
		Metadata: util.BuildConfigInfoMetadata(virtualService.ConfigMeta),
	}

	routeName := in.Name
	if match != nil && match.Name != "" {
		routeName = routeName + "." + match.Name
	}
	// add a name to the route
	out.Name = routeName

	operations := translateHeadersOperations(in.Headers)
	out.RequestHeadersToAdd = operations.requestHeadersToAdd
	out.ResponseHeadersToAdd = operations.responseHeadersToAdd
	out.RequestHeadersToRemove = operations.requestHeadersToRemove
	out.ResponseHeadersToRemove = operations.responseHeadersToRemove

	out.TypedPerFilterConfig = make(map[string]*any.Any)
	if redirect := in.Redirect; redirect != nil {
		action := &route.Route_Redirect{
			Redirect: &route.RedirectAction{
				HostRedirect: redirect.Authority,
				PathRewriteSpecifier: &route.RedirectAction_PathRedirect{
					PathRedirect: redirect.Uri,
				},
			}}

		switch in.Redirect.RedirectCode {
		case 0, 301:
			action.Redirect.ResponseCode = route.RedirectAction_MOVED_PERMANENTLY
		case 302:
			action.Redirect.ResponseCode = route.RedirectAction_FOUND
		case 303:
			action.Redirect.ResponseCode = route.RedirectAction_SEE_OTHER
		case 307:
			action.Redirect.ResponseCode = route.RedirectAction_TEMPORARY_REDIRECT
		case 308:
			action.Redirect.ResponseCode = route.RedirectAction_PERMANENT_REDIRECT
		default:
			log.Warnf("Redirect Code %d is not yet supported", in.Redirect.RedirectCode)
			action = nil
		}

		out.Action = action
	} else {
		action := &route.RouteAction{
			Cors:        translateCORSPolicy(in.CorsPolicy, node),
			RetryPolicy: retry.ConvertPolicy(in.Retries),
		}

		// Configure timeouts specified by Virtual Service if they are provided, otherwise set it to defaults.
		var d *duration.Duration
		if in.Timeout != nil {
			d = gogo.DurationToProtoDuration(in.Timeout)
		} else {
			d = features.DefaultRequestTimeout
		}

		action.Timeout = d
		action.MaxGrpcTimeout = d

		out.Action = &route.Route_Route{Route: action}

		if rewrite := in.Rewrite; rewrite != nil {
			action.PrefixRewrite = rewrite.Uri
			action.HostRewriteSpecifier = &route.RouteAction_HostRewriteLiteral{
				HostRewriteLiteral: rewrite.Authority,
			}
		}

		if in.Mirror != nil {
			if mp := mirrorPercent(in); mp != nil {
				action.RequestMirrorPolicies = []*route.RouteAction_RequestMirrorPolicy{{
					Cluster:         GetDestinationCluster(in.Mirror, serviceRegistry[host.Name(in.Mirror.Host)], port),
					RuntimeFraction: mp,
					TraceSampled:    &wrappers.BoolValue{Value: false},
				}}
			}
		}

		// TODO: eliminate this logic and use the total_weight option in envoy route
		weighted := make([]*route.WeightedCluster_ClusterWeight, 0)
		for _, dst := range in.Route {
			weight := &wrappers.UInt32Value{Value: uint32(dst.Weight)}
			if dst.Weight == 0 {
				// Ignore 0 weighted clusters if there are other clusters in the route.
				// But if this is the only cluster in the route, then add it as a cluster with weight 100
				if len(in.Route) == 1 {
					weight.Value = uint32(100)
				} else {
					continue
				}
			}

			operations := translateHeadersOperations(dst.Headers)

			hostname := host.Name(dst.GetDestination().GetHost())
			n := GetDestinationCluster(dst.Destination, serviceRegistry[hostname], port)

			clusterWeight := &route.WeightedCluster_ClusterWeight{
				Name:                    n,
				Weight:                  weight,
				RequestHeadersToAdd:     operations.requestHeadersToAdd,
				RequestHeadersToRemove:  operations.requestHeadersToRemove,
				ResponseHeadersToAdd:    operations.responseHeadersToAdd,
				ResponseHeadersToRemove: operations.responseHeadersToRemove,
			}

			weighted = append(weighted, clusterWeight)

			var configNamespace string
			if serviceRegistry[hostname] != nil {
				configNamespace = serviceRegistry[hostname].Attributes.Namespace
			}
			hashPolicy := getHashPolicy(push, node, dst, configNamespace)
			if hashPolicy != nil {
				action.HashPolicy = append(action.HashPolicy, hashPolicy)
			}
		}

		// rewrite to a single cluster if there is only weighted cluster
		if len(weighted) == 1 {
			action.ClusterSpecifier = &route.RouteAction_Cluster{Cluster: weighted[0].Name}
			out.RequestHeadersToAdd = append(out.RequestHeadersToAdd, weighted[0].RequestHeadersToAdd...)
			out.RequestHeadersToRemove = append(out.RequestHeadersToRemove, weighted[0].RequestHeadersToRemove...)
			out.ResponseHeadersToAdd = append(out.ResponseHeadersToAdd, weighted[0].ResponseHeadersToAdd...)
			out.ResponseHeadersToRemove = append(out.ResponseHeadersToRemove, weighted[0].ResponseHeadersToRemove...)
		} else {
			action.ClusterSpecifier = &route.RouteAction_WeightedClusters{
				WeightedClusters: &route.WeightedCluster{
					Clusters: weighted,
				},
			}
		}
	}

	out.Decorator = &route.Decorator{
		Operation: getRouteOperation(out, virtualService.Name, port),
	}
	if fault := in.Fault; fault != nil {
		out.TypedPerFilterConfig[wellknown.Fault] = util.MessageToAny(translateFault(in.Fault))
	}

	return out
}

// SortHeaderValueOption type and the functions below (Len, Less and Swap) are for sort.Stable for type HeaderValueOption
type SortHeaderValueOption []*core.HeaderValueOption

// mirrorPercent computes the mirror percent to be used based on "Mirror" data in route.
func mirrorPercent(in *networking.HTTPRoute) *core.RuntimeFractionalPercent {
	switch {
	case in.MirrorPercentage != nil:
		if in.MirrorPercentage.GetValue() > 0 {
			return &core.RuntimeFractionalPercent{
				DefaultValue: translatePercentToFractionalPercent(in.MirrorPercentage),
			}
		}
		// If zero percent is provided explicitly, we should not mirror.
		return nil
	case in.MirrorPercent != nil:
		if in.MirrorPercent.GetValue() > 0 {
			return &core.RuntimeFractionalPercent{
				DefaultValue: translateIntegerToFractionalPercent((int32(in.MirrorPercent.GetValue()))),
			}
		}
		// If zero percent is provided explicitly, we should not mirror.
		return nil
	default:
		// Default to 100 percent if percent is not given.
		return &core.RuntimeFractionalPercent{
			DefaultValue: translateIntegerToFractionalPercent(100),
		}
	}
}

// Len is i the sort.Interface for SortHeaderValueOption
func (b SortHeaderValueOption) Len() int {
	return len(b)
}

// Less is in the sort.Interface for SortHeaderValueOption
func (b SortHeaderValueOption) Less(i, j int) bool {
	if b[i] == nil || b[i].Header == nil {
		return false
	} else if b[j] == nil || b[j].Header == nil {
		return true
	}
	return strings.Compare(b[i].Header.Key, b[j].Header.Key) < 0
}

// Swap is in the sort.Interface for SortHeaderValueOption
func (b SortHeaderValueOption) Swap(i, j int) {
	b[i], b[j] = b[j], b[i]
}

// translateAppendHeaders translates headers
func translateAppendHeaders(headers map[string]string, appendFlag bool) []*core.HeaderValueOption {
	if len(headers) == 0 {
		return nil
	}
	headerValueOptionList := make([]*core.HeaderValueOption, 0, len(headers))
	for key, value := range headers {
		headerValueOptionList = append(headerValueOptionList, &core.HeaderValueOption{
			Header: &core.HeaderValue{
				Key:   key,
				Value: value,
			},
			Append: &wrappers.BoolValue{Value: appendFlag},
		})
	}
	sort.Stable(SortHeaderValueOption(headerValueOptionList))
	return headerValueOptionList
}

type headersOperations struct {
	requestHeadersToAdd     []*core.HeaderValueOption
	responseHeadersToAdd    []*core.HeaderValueOption
	requestHeadersToRemove  []string
	responseHeadersToRemove []string
}

// translateHeadersOperations translates headers operations
func translateHeadersOperations(headers *networking.Headers) headersOperations {
	req := headers.GetRequest()
	resp := headers.GetResponse()

	requestHeadersToAdd := translateAppendHeaders(req.GetSet(), false)
	requestHeadersToAdd = append(requestHeadersToAdd, translateAppendHeaders(req.GetAdd(), true)...)

	responseHeadersToAdd := translateAppendHeaders(resp.GetSet(), false)
	responseHeadersToAdd = append(responseHeadersToAdd, translateAppendHeaders(resp.GetAdd(), true)...)

	return headersOperations{
		requestHeadersToAdd:     requestHeadersToAdd,
		responseHeadersToAdd:    responseHeadersToAdd,
		requestHeadersToRemove:  append([]string{}, req.GetRemove()...), // copy slice
		responseHeadersToRemove: append([]string{}, resp.GetRemove()...),
	}
}

// translateRouteMatch translates match condition
func translateRouteMatch(in *networking.HTTPMatchRequest, node *model.Proxy) *route.RouteMatch {
	out := &route.RouteMatch{PathSpecifier: &route.RouteMatch_Prefix{Prefix: "/"}}
	if in == nil {
		return out
	}

	for name, stringMatch := range in.Headers {
		matcher := translateHeaderMatch(name, stringMatch, node)
		out.Headers = append(out.Headers, matcher)
	}

	for name, stringMatch := range in.WithoutHeaders {
		matcher := translateHeaderMatch(name, stringMatch, node)
		matcher.InvertMatch = true
		out.Headers = append(out.Headers, matcher)
	}

	// guarantee ordering of headers
	sort.Slice(out.Headers, func(i, j int) bool {
		return out.Headers[i].Name < out.Headers[j].Name
	})

	if in.Uri != nil {
		switch m := in.Uri.MatchType.(type) {
		case *networking.StringMatch_Exact:
			out.PathSpecifier = &route.RouteMatch_Path{Path: m.Exact}
		case *networking.StringMatch_Prefix:
			out.PathSpecifier = &route.RouteMatch_Prefix{Prefix: m.Prefix}
		case *networking.StringMatch_Regex:
			out.PathSpecifier = &route.RouteMatch_SafeRegex{
				SafeRegex: &matcher.RegexMatcher{
					// nolint: staticcheck
					EngineType: regexMatcher(node),
					Regex:      m.Regex,
				},
			}
		}
	}

	out.CaseSensitive = &wrappers.BoolValue{Value: !in.IgnoreUriCase}

	if in.Method != nil {
		matcher := translateHeaderMatch(HeaderMethod, in.Method, node)
		out.Headers = append(out.Headers, matcher)
	}

	if in.Authority != nil {
		matcher := translateHeaderMatch(HeaderAuthority, in.Authority, node)
		out.Headers = append(out.Headers, matcher)
	}

	if in.Scheme != nil {
		matcher := translateHeaderMatch(HeaderScheme, in.Scheme, node)
		out.Headers = append(out.Headers, matcher)
	}

	for name, stringMatch := range in.QueryParams {
		matcher := translateQueryParamMatch(name, stringMatch, node)
		out.QueryParameters = append(out.QueryParameters, matcher)
	}

	return out
}

// translateQueryParamMatch translates a StringMatch to a QueryParameterMatcher.
func translateQueryParamMatch(name string, in *networking.StringMatch, node *model.Proxy) *route.QueryParameterMatcher {
	out := &route.QueryParameterMatcher{
		Name: name,
	}

	switch m := in.MatchType.(type) {
	case *networking.StringMatch_Exact:
		out.QueryParameterMatchSpecifier = &route.QueryParameterMatcher_StringMatch{
			StringMatch: &matcher.StringMatcher{MatchPattern: &matcher.StringMatcher_Exact{Exact: m.Exact}},
		}
	case *networking.StringMatch_Regex:
		out.QueryParameterMatchSpecifier = &route.QueryParameterMatcher_StringMatch{
			StringMatch: &matcher.StringMatcher{MatchPattern: &matcher.StringMatcher_SafeRegex{
				SafeRegex: &matcher.RegexMatcher{
					EngineType: regexMatcher(node),
					Regex:      m.Regex,
				},
			},
			}}
	}

	return out
}

// isCatchAllHeaderMatch determines if the given header is matched with all strings or not.
// Currently, if the regex has "*" value, it returns true
func isCatchAllHeaderMatch(in *networking.StringMatch) bool {
	catchall := false

	if in == nil {
		return true
	}

	switch m := in.MatchType.(type) {
	case *networking.StringMatch_Regex:
		catchall = m.Regex == "*"
	}

	return catchall
}

// translateHeaderMatch translates to HeaderMatcher
func translateHeaderMatch(name string, in *networking.StringMatch, node *model.Proxy) *route.HeaderMatcher {
	out := &route.HeaderMatcher{
		Name: name,
	}

	if isCatchAllHeaderMatch(in) {
		out.HeaderMatchSpecifier = &route.HeaderMatcher_PresentMatch{PresentMatch: true}
		return out
	}

	switch m := in.MatchType.(type) {
	case *networking.StringMatch_Exact:
		out.HeaderMatchSpecifier = &route.HeaderMatcher_ExactMatch{ExactMatch: m.Exact}
	case *networking.StringMatch_Prefix:
		// Envoy regex grammar is RE2 (https://github.com/google/re2/wiki/Syntax)
		// Golang has a slightly different regex grammar
		out.HeaderMatchSpecifier = &route.HeaderMatcher_PrefixMatch{PrefixMatch: m.Prefix}
	case *networking.StringMatch_Regex:
		out.HeaderMatchSpecifier = &route.HeaderMatcher_SafeRegexMatch{
			SafeRegexMatch: &matcher.RegexMatcher{
				EngineType: regexMatcher(node),
				Regex:      m.Regex,
			},
		}
	}

	return out
}

func convertToExactEnvoyMatch(in []string) []*matcher.StringMatcher {
	res := make([]*matcher.StringMatcher, 0, len(in))

	for _, istioMatcher := range in {
		res = append(res, &matcher.StringMatcher{MatchPattern: &matcher.StringMatcher_Exact{Exact: istioMatcher}})
	}

	return res
}

func convertToEnvoyMatch(in []*networking.StringMatch, node *model.Proxy) []*matcher.StringMatcher {
	res := make([]*matcher.StringMatcher, 0, len(in))

	for _, istioMatcher := range in {
		switch m := istioMatcher.MatchType.(type) {
		case *networking.StringMatch_Exact:
			res = append(res, &matcher.StringMatcher{MatchPattern: &matcher.StringMatcher_Exact{Exact: m.Exact}})
		case *networking.StringMatch_Prefix:
			res = append(res, &matcher.StringMatcher{MatchPattern: &matcher.StringMatcher_Prefix{Prefix: m.Prefix}})
		case *networking.StringMatch_Regex:
			res = append(res, &matcher.StringMatcher{MatchPattern: &matcher.StringMatcher_SafeRegex{
				SafeRegex: &matcher.RegexMatcher{
					EngineType: regexMatcher(node),
					Regex:      m.Regex,
				},
			},
			})
		}

	}

	return res
}

// translateCORSPolicy translates CORS policy
func translateCORSPolicy(in *networking.CorsPolicy, node *model.Proxy) *route.CorsPolicy {
	if in == nil {
		return nil
	}

	// CORS filter is enabled by default
	out := route.CorsPolicy{}
	if in.AllowOrigins != nil {
		out.AllowOriginStringMatch = convertToEnvoyMatch(in.AllowOrigins, node)
	} else if in.AllowOrigin != nil {
		out.AllowOriginStringMatch = convertToExactEnvoyMatch(in.AllowOrigin)
	}

	out.EnabledSpecifier = &route.CorsPolicy_FilterEnabled{
		FilterEnabled: &core.RuntimeFractionalPercent{
			DefaultValue: &xdstype.FractionalPercent{
				Numerator:   100,
				Denominator: xdstype.FractionalPercent_HUNDRED,
			},
		},
	}

	out.AllowCredentials = gogo.BoolToProtoBool(in.AllowCredentials)
	out.AllowHeaders = strings.Join(in.AllowHeaders, ",")
	out.AllowMethods = strings.Join(in.AllowMethods, ",")
	out.ExposeHeaders = strings.Join(in.ExposeHeaders, ",")
	if in.MaxAge != nil {
		out.MaxAge = strconv.FormatInt(in.MaxAge.GetSeconds(), 10)
	}
	return &out
}

// getRouteOperation returns readable route description for trace.
func getRouteOperation(in *route.Route, vsName string, port int) string {
	path := "/*"
	m := in.GetMatch()
	ps := m.GetPathSpecifier()
	if ps != nil {
		switch ps.(type) {
		case *route.RouteMatch_Prefix:
			path = m.GetPrefix() + "*"
		case *route.RouteMatch_Path:
			path = m.GetPath()
		case *route.RouteMatch_SafeRegex:
			path = m.GetSafeRegex().GetRegex()
		}
	}

	// If there is only one destination cluster in route, return host:port/uri as description of route.
	// Otherwise there are multiple destination clusters and destination host is not clear. For that case
	// return virtual serivce name:port/uri as substitute.
	if c := in.GetRoute().GetCluster(); model.IsValidSubsetKey(c) {
		// Parse host and port from cluster name.
		_, _, h, p := model.ParseSubsetKey(c)
		return string(h) + ":" + strconv.Itoa(p) + path
	}
	return vsName + ":" + strconv.Itoa(port) + path
}

// BuildDefaultHTTPInboundRoute builds a default inbound route.
func BuildDefaultHTTPInboundRoute(node *model.Proxy, clusterName string, operation string) *route.Route {
	notimeout := ptypes.DurationProto(0)

	val := &route.Route{
		Match: translateRouteMatch(nil, node),
		Decorator: &route.Decorator{
			Operation: operation,
		},
		Action: &route.Route_Route{
			Route: &route.RouteAction{
				ClusterSpecifier: &route.RouteAction_Cluster{Cluster: clusterName},
				Timeout:          notimeout,
				MaxGrpcTimeout:   notimeout,
			},
		},
	}

	val.Name = DefaultRouteName
	return val
}

// BuildDefaultHTTPOutboundRoute builds a default outbound route, including a retry policy.
func BuildDefaultHTTPOutboundRoute(node *model.Proxy, clusterName string, operation string) *route.Route {
	// Start with the same configuration as for inbound.
	out := BuildDefaultHTTPInboundRoute(node, clusterName, operation)

	// Add a default retry policy for outbound routes.
	out.GetRoute().RetryPolicy = retry.DefaultPolicy()
	return out
}

// translatePercentToFractionalPercent translates an v1alpha3 Percent instance
// to an envoy.type.FractionalPercent instance.
func translatePercentToFractionalPercent(p *networking.Percent) *xdstype.FractionalPercent {
	return &xdstype.FractionalPercent{
		Numerator:   uint32(p.Value * 10000),
		Denominator: xdstype.FractionalPercent_MILLION,
	}
}

// translateIntegerToFractionalPercent translates an int32 instance to an
// envoy.type.FractionalPercent instance.
func translateIntegerToFractionalPercent(p int32) *xdstype.FractionalPercent {
	return &xdstype.FractionalPercent{
		Numerator:   uint32(p),
		Denominator: xdstype.FractionalPercent_HUNDRED,
	}
}

// translateFault translates networking.HTTPFaultInjection into Envoy's HTTPFault
func translateFault(in *networking.HTTPFaultInjection) *xdshttpfault.HTTPFault {
	if in == nil {
		return nil
	}

	out := xdshttpfault.HTTPFault{}
	if in.Delay != nil {
		out.Delay = &xdsfault.FaultDelay{}
		if in.Delay.Percentage != nil {
			out.Delay.Percentage = translatePercentToFractionalPercent(in.Delay.Percentage)
		} else {
			out.Delay.Percentage = translateIntegerToFractionalPercent(in.Delay.Percent)
		}
		switch d := in.Delay.HttpDelayType.(type) {
		case *networking.HTTPFaultInjection_Delay_FixedDelay:
			out.Delay.FaultDelaySecifier = &xdsfault.FaultDelay_FixedDelay{
				FixedDelay: gogo.DurationToProtoDuration(d.FixedDelay),
			}
		default:
			log.Warnf("Exponential faults are not yet supported")
			out.Delay = nil
		}
	}

	if in.Abort != nil {
		out.Abort = &xdshttpfault.FaultAbort{}
		if in.Abort.Percentage != nil {
			out.Abort.Percentage = translatePercentToFractionalPercent(in.Abort.Percentage)
		}
		switch a := in.Abort.ErrorType.(type) {
		case *networking.HTTPFaultInjection_Abort_HttpStatus:
			out.Abort.ErrorType = &xdshttpfault.FaultAbort_HttpStatus{
				HttpStatus: uint32(a.HttpStatus),
			}
		default:
			log.Warnf("Non-HTTP type abort faults are not yet supported")
			out.Abort = nil
		}
	}

	if out.Delay == nil && out.Abort == nil {
		return nil
	}

	return &out
}

func portLevelSettingsConsistentHash(dst *networking.Destination,
	pls []*networking.TrafficPolicy_PortTrafficPolicy) *networking.LoadBalancerSettings_ConsistentHashLB {
	if dst.Port != nil {
		portNumber := dst.GetPort().GetNumber()
		for _, setting := range pls {
			number := setting.GetPort().GetNumber()
			if number == portNumber {
				return setting.GetLoadBalancer().GetConsistentHash()
			}
		}
	}

	return nil
}

func consistentHashToHashPolicy(consistentHash *networking.LoadBalancerSettings_ConsistentHashLB) *route.RouteAction_HashPolicy {
	switch consistentHash.GetHashKey().(type) {
	case *networking.LoadBalancerSettings_ConsistentHashLB_HttpHeaderName:
		return &route.RouteAction_HashPolicy{
			PolicySpecifier: &route.RouteAction_HashPolicy_Header_{
				Header: &route.RouteAction_HashPolicy_Header{
					HeaderName: consistentHash.GetHttpHeaderName(),
				},
			},
		}
	case *networking.LoadBalancerSettings_ConsistentHashLB_HttpCookie:
		cookie := consistentHash.GetHttpCookie()
		var ttl *duration.Duration
		if cookie.GetTtl() != nil {
			ttl = gogo.DurationToProtoDuration(cookie.GetTtl())
		}
		return &route.RouteAction_HashPolicy{
			PolicySpecifier: &route.RouteAction_HashPolicy_Cookie_{
				Cookie: &route.RouteAction_HashPolicy_Cookie{
					Name: cookie.GetName(),
					Ttl:  ttl,
					Path: cookie.GetPath(),
				},
			},
		}
	case *networking.LoadBalancerSettings_ConsistentHashLB_UseSourceIp:
		return &route.RouteAction_HashPolicy{
			PolicySpecifier: &route.RouteAction_HashPolicy_ConnectionProperties_{
				ConnectionProperties: &route.RouteAction_HashPolicy_ConnectionProperties{
					SourceIp: consistentHash.GetUseSourceIp(),
				},
			},
		}
	case *networking.LoadBalancerSettings_ConsistentHashLB_HttpQueryParameterName:
		return &route.RouteAction_HashPolicy{
			PolicySpecifier: &route.RouteAction_HashPolicy_QueryParameter_{
				QueryParameter: &route.RouteAction_HashPolicy_QueryParameter{
					Name: consistentHash.GetHttpQueryParameterName(),
				},
			},
		}
	}
	return nil
}

func getHashPolicyByService(node *model.Proxy, push *model.PushContext, svc *model.Service, port *model.Port) *route.RouteAction_HashPolicy {
	if push == nil {
		return nil
	}
	destinationRule := push.DestinationRule(node, svc)
	if destinationRule == nil {
		return nil
	}
	rule := destinationRule.Spec.(*networking.DestinationRule)
	consistentHash := rule.GetTrafficPolicy().GetLoadBalancer().GetConsistentHash()
	portLevelSettings := rule.GetTrafficPolicy().GetPortLevelSettings()
	for _, setting := range portLevelSettings {
		number := setting.GetPort().GetNumber()
		if int(number) == port.Port {
			consistentHash = setting.GetLoadBalancer().GetConsistentHash()
			break
		}
	}
	return consistentHashToHashPolicy(consistentHash)
}

func getHashPolicy(push *model.PushContext, node *model.Proxy, dst *networking.HTTPRouteDestination,
	configNamespace string) *route.RouteAction_HashPolicy {
	if push == nil {
		return nil
	}

	destination := dst.GetDestination()
	destinationRule := push.DestinationRule(node,
		&model.Service{
			Hostname:   host.Name(destination.Host),
			Attributes: model.ServiceAttributes{Namespace: configNamespace},
		})
	if destinationRule == nil {
		return nil
	}
	rule := destinationRule.Spec.(*networking.DestinationRule)

	consistentHash := rule.GetTrafficPolicy().GetLoadBalancer().GetConsistentHash()
	portLevelSettings := rule.GetTrafficPolicy().GetPortLevelSettings()
	plsHash := portLevelSettingsConsistentHash(destination, portLevelSettings)

	var subsetHash, subsetPLSHash *networking.LoadBalancerSettings_ConsistentHashLB
	for _, subset := range rule.GetSubsets() {
		if subset.GetName() == destination.GetSubset() {
			subsetPortLevelSettings := subset.GetTrafficPolicy().GetPortLevelSettings()
			subsetHash = subset.GetTrafficPolicy().GetLoadBalancer().GetConsistentHash()
			subsetPLSHash = portLevelSettingsConsistentHash(destination, subsetPortLevelSettings)

			break
		}
	}

	switch {
	case subsetPLSHash != nil:
		consistentHash = subsetPLSHash
	case subsetHash != nil:
		consistentHash = subsetHash
	case plsHash != nil:
		consistentHash = plsHash
	}
	return consistentHashToHashPolicy(consistentHash)
}

// catchAllMatch returns a catch all match block if available in the route, otherwise returns nil.
func catchAllMatch(http *networking.HTTPRoute) *networking.HTTPMatchRequest {
	for _, match := range http.Match {
		if isCatchAllMatch(match) {
			return match
		}
	}
	return nil
}

// isCatchAll returns true if HTTPMatchRequest is a catchall match otherwise false.
func isCatchAllMatch(m *networking.HTTPMatchRequest) bool {
	catchall := false
	if m.Uri != nil {
		switch m := m.Uri.MatchType.(type) {
		case *networking.StringMatch_Prefix:
			catchall = m.Prefix == "/"
		case *networking.StringMatch_Regex:
			catchall = m.Regex == "*"
		}
	}
	// A Match is catch all if and only if it has no header/query param match
	// and URI has a prefix / or regex *.
	return catchall && len(m.Headers) == 0 && len(m.QueryParams) == 0
}

// CombineVHostRoutes semi concatenates Vhost's routes into a single route set.
// Moves the catch all routes alone to the end, while retaining
// the relative order of other routes in the concatenated route.
// Assumes that the virtual services that generated first and second are ordered by
// time.
func CombineVHostRoutes(routeSets ...[]*route.Route) []*route.Route {
	l := 0
	for _, rs := range routeSets {
		l += len(rs)
	}
	allroutes := make([]*route.Route, 0, l)
	catchAllRoutes := make([]*route.Route, 0)
	for _, routes := range routeSets {
		for _, r := range routes {
			if isCatchAllRoute(r) {
				catchAllRoutes = append(catchAllRoutes, r)
			} else {
				allroutes = append(allroutes, r)
			}
		}
	}
	return append(allroutes, catchAllRoutes...)
}

// isCatchAllRoute returns true if an Envoy route is a catchall route otherwise false.
func isCatchAllRoute(r *route.Route) bool {
	catchall := false
	switch ir := r.Match.PathSpecifier.(type) {
	case *route.RouteMatch_Prefix:
		catchall = ir.Prefix == "/"
	case *route.RouteMatch_SafeRegex:
		catchall = ir.SafeRegex.GetRegex() == "*"
	}
	// A Match is catch all if and only if it has no header/query param match
	// and URI has a prefix / or regex *.
	return catchall && len(r.Match.Headers) == 0 && len(r.Match.QueryParameters) == 0
}

func traceOperation(host string, port int) string {
	// Format : "%s:%d/*"
	return host + ":" + strconv.Itoa(port) + "/*"
}

func regexMatcher(node *model.Proxy) *matcher.RegexMatcher_GoogleRe2 {
	return regexEngine
}
