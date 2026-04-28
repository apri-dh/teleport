/*
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package services

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/gravitational/trace"
	"golang.org/x/net/idna"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/types/wrappers"
	"github.com/gravitational/teleport/api/utils/clientutils"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/utils"
)

// AppGetter defines interface for fetching application resources.
type AppGetter interface {
	// GetApps returns all application resources.
	GetApps(context.Context) ([]types.Application, error)
	// ListApps returns a page of application resources.
	ListApps(ctx context.Context, limit int, startKey string) ([]types.Application, string, error)
	// Apps returns application resources within the range [start, end).
	Apps(ctx context.Context, start, end string) iter.Seq2[types.Application, error]
	// GetApp returns the specified application resource.
	GetApp(ctx context.Context, name string) (types.Application, error)
}

// Applications defines an interface for managing application resources.
type Applications interface {
	// AppGetter provides methods for fetching application resources.
	AppGetter
	// CreateApp creates a new application resource.
	CreateApp(context.Context, types.Application) error
	// UpdateApp updates an existing application resource.
	UpdateApp(context.Context, types.Application) error
	// DeleteApp removes the specified application resource.
	DeleteApp(ctx context.Context, name string) error
	// DeleteAllApps removes all database resources.
	DeleteAllApps(context.Context) error
}

// ApplicationsInternal extends the Access interface with auth-specific internal methods.
type ApplicationsInternal interface {
	Applications

	// AppendPutAppActions adds conditional actions to an atomic write to create
	// or update an application resource.
	AppendPutAppActions(
		actions []backend.ConditionalAction,
		app types.Application,
		condition backend.Condition,
	) ([]backend.ConditionalAction, error)

	// AppendDeleteAppActions adds conditional actions to an atomic write to
	// delete an application resource.
	AppendDeleteAppActions(
		actions []backend.ConditionalAction,
		name string,
		condition backend.Condition,
	) ([]backend.ConditionalAction, error)
}

// ValidateApp validates the Application resource. It does not modify the
// app's name: changing the name in-place would silently rewrite the backend
// resource key during UpdateApp, which would target the wrong record on
// clusters that already contain mixed-case app names from earlier versions.
// Heartbeat callers that need to accept mixed-case names from older agents
// must lowercase the name themselves before calling ValidateApp.
//
// On success, ValidateApp lowercases the app's RequiredAppNames in place.
// RequiredAppNames is a spec field, not the resource key, so rewriting it
// does not affect which backend record is targeted; it ensures references
// match the lowercased names of the apps they reference. The lowercase
// runs only on the validated path so a rejected app does not leave with
// partially mutated state.
func ValidateApp(app types.Application, proxyGetter ProxyGetter) error {
	// Validate that the app name is a valid DNS subdomain (RFC 1123). App
	// names become subdomains (appName.proxyHost), so each label must be
	// lowercase alphanumeric or hyphens. Dots are allowed because some
	// integrations (e.g. AWS OIDC) use dotted names like "env.prod".
	if errs := validation.IsDNS1123Subdomain(app.GetName()); len(errs) > 0 {
		return trace.BadParameter("application name %q must be a valid DNS name (lowercase alphanumeric, '-', or '.', must start and end with alphanumeric, max 253 chars): https://goteleport.com/docs/enroll-resources/application-access/guides/connecting-apps/#application-name", app.GetName())
	}

	// If no public address is set, there's nothing else to validate.
	if app.GetPublicAddr() == "" {
		lowercaseRequiredAppNames(app)
		return nil
	}

	addr := app.GetPublicAddr()
	// Reject public_addr values that contain a URI scheme.
	if strings.Contains(addr, "://") {
		return trace.BadParameter("application %q public_addr %q must not contain a URI scheme; use a bare hostname", app.GetName(), addr)
	}
	// Reject public_addr values that carry a path, query, fragment, or
	// userinfo. utils.ParseAddr accepts these but they are not valid in
	// a bare hostname and would produce an invalid routing or cert
	// hostname downstream.
	if strings.ContainsAny(addr, "/?#@") {
		return trace.BadParameter("application %q public_addr %q must be a bare hostname; remove any path, query, fragment, or userinfo", app.GetName(), addr)
	}
	// Reject public_addr values that contain a port.
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return trace.BadParameter("application %q public_addr %q must not contain a port, applications will be available on the same port as the web proxy", app.GetName(), addr)
	}
	// Reject public_addr values that are IP addresses, including bracketed
	// IPv6 like [::1]. Strip a paired set of brackets only.
	stripped := addr
	if strings.HasPrefix(stripped, "[") && strings.HasSuffix(stripped, "]") {
		stripped = stripped[1 : len(stripped)-1]
	}
	if net.ParseIP(stripped) != nil {
		return trace.BadParameter("application %q public_addr %q must not be an IP address, Teleport Application Access uses DNS names for routing", app.GetName(), addr)
	}
	// The shape checks above run on every write path because the heartbeat
	// handler in lib/auth/grpcserver.go reaches ValidateApp without going
	// through CheckAndSetDefaults. The block below detects conflicts with
	// proxy public addresses, which require fetching proxy state separately.
	appAddr, err := utils.ParseAddr(app.GetPublicAddr())
	if err != nil {
		return trace.Wrap(err)
	}

	// Convert the application's public address hostname to its ASCII representation for comparison. Strip any trailing
	// dots to ensure consistent comparison.
	asciiAppHostname, err := idna.ToASCII(strings.TrimRight(appAddr.Host(), "."))
	if err != nil {
		return trace.Wrap(err, "app %q has an invalid IDN hostname %q", app.GetName(), appAddr.Host())
	}

	proxyServers, err := clientutils.CollectWithFallback(context.TODO(), proxyGetter.ListProxyServers, func(context.Context) ([]types.Server, error) {
		//nolint:staticcheck // TODO(kiosion) DELETE IN 21.0.0
		return proxyGetter.GetProxies()
	})
	if err != nil {
		return trace.Wrap(err)
	}

	// Prevent routing conflicts and session hijacking by ensuring the application's public address does not match the
	// public address of any proxy. If an application shares a public address with a proxy, requests intended for the
	// proxy could be misrouted to the application, compromising security.
	for _, proxyServer := range proxyServers {
		proxyAddrs, err := utils.ParseAddrs(proxyServer.GetPublicAddrs())
		if err != nil {
			return trace.Wrap(err)
		}

		for _, proxyAddr := range proxyAddrs {
			// Also convert the proxy's public address hostname to its ASCII representation for comparison and strip any
			// trailing dots.
			asciiProxyHostname, err := idna.ToASCII(strings.TrimRight(proxyAddr.Host(), "."))
			if err != nil {
				return trace.Wrap(err, "proxy %q has an invalid IDN hostname %q", proxyServer.GetName(), proxyAddr)
			}

			// Compare the ASCII-normalized hostnames for equality, ignoring case.
			if strings.EqualFold(asciiProxyHostname, asciiAppHostname) {
				return trace.BadParameter(
					"Application %q public address %q conflicts with the Teleport Proxy public address. "+
						"Configure the application to use a unique public address that does not match the proxy's public addresses. "+
						"Refer to https://goteleport.com/docs/enroll-resources/application-access/guides/connecting-apps/#customize-public-address.",
					app.GetName(),
					app.GetPublicAddr(),
				)
			}
		}
	}

	lowercaseRequiredAppNames(app)
	return nil
}

// lowercaseRequiredAppNames lowercases the app's RequiredAppNames in place.
// Required-app references look up other apps by their lowercased names, so
// the references must be lowercased too. Use a local copy of the slice to
// make the mutation explicit at the call site.
func lowercaseRequiredAppNames(app types.Application) {
	required := app.GetRequiredAppNames()
	for i, n := range required {
		required[i] = strings.ToLower(n)
	}
}

// NormalizeAppServerForHeartbeat rewrites the inner app name and public
// address heartbeated by an older agent into the bare-hostname,
// lowercase form ValidateApp now requires. It is shared by the gRPC
// handler and the inventory control stream so both heartbeat paths
// apply the same normalization; without this the storage key and
// routing identity could diverge across paths. Admin-facing paths must
// not call this helper: they reject mixed-case names instead of
// rewriting them, to avoid silently retargeting an existing record.
//
// The lowercase is applied to both the inner app name and the outer
// AppServer metadata name. The outer name is rewritten whenever it
// case-folds to the inner name so an older agent that only lowercased
// one of the two still ends up with both lowercased after this helper
// runs. A true mismatch between outer and inner names is left
// unchanged so it surfaces in ValidateApp instead of being silently
// rewritten in only one place.
func NormalizeAppServerForHeartbeat(server types.AppServer) {
	app := server.GetApp()
	if app == nil {
		return
	}
	innerName := strings.ToLower(app.GetName())
	if innerName != app.GetName() {
		app.SetName(innerName)
	}
	if outerName := server.GetName(); strings.EqualFold(outerName, innerName) && outerName != innerName {
		server.SetName(innerName)
	}
	if normalised := normalizeHeartbeatPublicAddr(app.GetPublicAddr()); normalised != app.GetPublicAddr() {
		app.SetPublicAddr(normalised)
	}
}

// normalizeHeartbeatPublicAddr rewrites a public address heartbeated by
// an older agent into the bare-hostname form ValidateApp now requires.
// It strips a leading URL scheme and path, strips a trailing port, and
// returns the result. Inputs that are already bare hostnames pass
// through unchanged. Inputs that cannot be normalised (for example IP
// addresses, opaque URLs like mailto:, or empty values) are returned
// as-is so that ValidateApp produces the same error it would on the
// admin-facing CreateApp and UpdateApp paths.
func normalizeHeartbeatPublicAddr(addr string) string {
	if addr == "" {
		return addr
	}
	// Strip a URL scheme and path if present, e.g.
	// "https://app.example.com/path" -> "app.example.com".
	// url.Hostname() also strips a port, so https://host:8443/path
	// becomes "host" in this branch.
	if strings.Contains(addr, "://") {
		if u, err := url.Parse(addr); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
		return addr
	}
	// Strip a trailing port if present, e.g. "app.example.com:443"
	// -> "app.example.com". Older agent versions emitted the proxy's
	// port in app public_addr values.
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// MarshalApp marshals Application resource to JSON.
func MarshalApp(app types.Application, opts ...MarshalOption) ([]byte, error) {
	cfg, err := CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	switch app := app.(type) {
	case *types.AppV3:
		if err := app.CheckAndSetDefaults(); err != nil {
			return nil, trace.Wrap(err)
		}

		return utils.FastMarshal(maybeResetProtoRevision(cfg.PreserveRevision, app))
	default:
		return nil, trace.BadParameter("unsupported app resource %T", app)
	}
}

// UnmarshalApp unmarshals Application resource from JSON.
func UnmarshalApp(data []byte, opts ...MarshalOption) (types.Application, error) {
	if len(data) == 0 {
		return nil, trace.BadParameter("missing app resource data")
	}
	cfg, err := CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var h types.ResourceHeader
	if err := utils.FastUnmarshal(data, &h); err != nil {
		return nil, trace.Wrap(err)
	}
	switch h.Version {
	case types.V3:
		var app types.AppV3
		if err := utils.FastUnmarshal(data, &app); err != nil {
			return nil, trace.BadParameter("%s", err)
		}
		if err := app.CheckAndSetDefaults(); err != nil {
			return nil, trace.Wrap(err)
		}
		if cfg.Revision != "" {
			app.SetRevision(cfg.Revision)
		}
		if !cfg.Expires.IsZero() {
			app.SetExpiry(cfg.Expires)
		}
		return &app, nil
	}
	return nil, trace.BadParameter("unsupported app resource version %q", h.Version)
}

// MarshalAppServer marshals the AppServer resource to JSON.
func MarshalAppServer(appServer types.AppServer, opts ...MarshalOption) ([]byte, error) {
	cfg, err := CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	switch appServer := appServer.(type) {
	case *types.AppServerV3:
		if err := appServer.CheckAndSetDefaults(); err != nil {
			return nil, trace.Wrap(err)
		}

		return utils.FastMarshal(maybeResetProtoRevision(cfg.PreserveRevision, appServer))
	default:
		return nil, trace.BadParameter("unsupported app server resource %T", appServer)
	}
}

// UnmarshalAppServer unmarshals AppServer resource from JSON.
func UnmarshalAppServer(data []byte, opts ...MarshalOption) (types.AppServer, error) {
	if len(data) == 0 {
		return nil, trace.BadParameter("missing app server data")
	}
	cfg, err := CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var h types.ResourceHeader
	if err := utils.FastUnmarshal(data, &h); err != nil {
		return nil, trace.Wrap(err)
	}
	switch h.Version {
	case types.V3:
		var s types.AppServerV3
		if err := utils.FastUnmarshal(data, &s); err != nil {
			return nil, trace.BadParameter("%s", err)
		}
		if err := s.CheckAndSetDefaults(); err != nil {
			return nil, trace.Wrap(err)
		}
		if cfg.Revision != "" {
			s.SetRevision(cfg.Revision)
		}
		if !cfg.Expires.IsZero() {
			s.SetExpiry(cfg.Expires)
		}
		return &s, nil
	}
	return nil, trace.BadParameter("unsupported app server resource version %q", h.Version)
}

// NewApplicationFromKubeService creates application resources from kubernetes service.
// It transforms service fields and annotations into appropriate Teleport app fields.
// Service labels are copied to app labels.
func NewApplicationFromKubeService(service corev1.Service, clusterName, protocol string, port corev1.ServicePort) (types.Application, error) {
	appURI := buildAppURI(protocol, GetServiceFQDN(service), service.GetAnnotations()[types.DiscoveryPathLabel], port.Port)

	rewriteConfig, err := getAppRewriteConfig(service.GetAnnotations())
	if err != nil {
		return nil, trace.Wrap(err, "could not get app rewrite config for the service")
	}

	appNameAnnotation := service.GetAnnotations()[types.DiscoveryAppNameLabel]
	appName, err := getAppName(service.GetName(), service.GetNamespace(), clusterName, port.Name, appNameAnnotation)
	if err != nil {
		return nil, trace.Wrap(err, "could not create app name for the service")
	}

	labels, err := getAppLabels(service.GetLabels(), clusterName)
	if err != nil {
		return nil, trace.Wrap(err, "could not get labels for the service")
	}

	app, err := types.NewAppV3(types.Metadata{
		Name: appName,
		Description: cmp.Or(
			getDescription(service.GetAnnotations()),
			fmt.Sprintf("Discovered application in Kubernetes cluster %q", clusterName),
		),
		Labels: labels,
	}, types.AppSpecV3{
		URI:                appURI,
		Rewrite:            rewriteConfig,
		InsecureSkipVerify: getTLSInsecureSkipVerify(service.GetAnnotations()),
		PublicAddr:         getPublicAddr(service.GetAnnotations()),
	})
	if err != nil {
		return nil, trace.Wrap(err, "could not create an app from Kubernetes service")
	}

	return app, nil
}

// GetServiceFQDN returns the fully qualified domain name for the service.
func GetServiceFQDN(service corev1.Service) string {
	// If service type is ExternalName it points to external DNS name, to keep correct
	// HOST for HTTP requests we return already final external DNS name.
	// https://kubernetes.io/docs/concepts/services-networking/service/#externalname
	if service.Spec.Type == corev1.ServiceTypeExternalName {
		return service.Spec.ExternalName
	}
	return fmt.Sprintf("%s.%s.svc.%s", service.GetName(), service.GetNamespace(), clusterDomainResolver())
}

func buildAppURI(protocol, serviceFQDN, path string, port int32) string {
	return (&url.URL{
		Scheme: protocol,
		Host:   net.JoinHostPort(serviceFQDN, strconv.Itoa(int(port))),
		Path:   path,
	}).String()
}

func getAppRewriteConfig(annotations map[string]string) (*types.Rewrite, error) {
	rewritePayload := annotations[types.DiscoveryAppRewriteLabel]
	if rewritePayload == "" {
		return nil, nil
	}

	rw := types.Rewrite{}
	reader := strings.NewReader(rewritePayload)
	decoder := kyaml.NewYAMLOrJSONDecoder(reader, 32*1024)
	err := decoder.Decode(&rw)
	if err != nil {
		return nil, trace.Wrap(err, "failed decoding rewrite config")
	}

	return &rw, nil
}

func getDescription(annotations map[string]string) string {
	return annotations[types.DiscoveryDescription]
}

func getPublicAddr(annotations map[string]string) string {
	return annotations[types.DiscoveryPublicAddr]
}

func getTLSInsecureSkipVerify(annotations map[string]string) bool {
	val := annotations[types.DiscoveryAppInsecureSkipVerify]
	if val == "" {
		return false
	}
	return val == "true"
}

func getAppName(serviceName, namespace, clusterName, portName, nameAnnotation string) (string, error) {
	if nameAnnotation != "" {
		name := nameAnnotation
		if portName != "" {
			name = fmt.Sprintf("%s-%s", name, portName)
		}

		if len(validation.IsDNS1123Label(name)) > 0 {
			return "", trace.BadParameter(
				"application name %q must be a valid DNS label (lowercase alphanumeric or '-', must start and end with alphanumeric, max 63 chars): https://goteleport.com/docs/enroll-resources/application-access/guides/connecting-apps/#application-name", name)
		}

		return name, nil
	}

	// clusterName comes from the operator-set discovery_group, which is a
	// free-form string. Lowercase it (and replace dots) so the composed app
	// name passes the DNS-1123 subdomain rule that services.ValidateApp now
	// enforces on every write path.
	clusterName = strings.ToLower(strings.ReplaceAll(clusterName, ".", "-"))
	if portName != "" {
		return fmt.Sprintf("%s-%s-%s-%s", serviceName, portName, namespace, clusterName), nil
	}
	return fmt.Sprintf("%s-%s-%s", serviceName, namespace, clusterName), nil
}

func getAppLabels(serviceLabels map[string]string, clusterName string) (map[string]string, error) {
	result := make(map[string]string, len(serviceLabels)+1)

	for k, v := range serviceLabels {
		if !types.IsValidLabelKey(k) {
			return nil, trace.BadParameter("invalid label key: %q", k)
		}

		result[k] = v
	}
	result[types.KubernetesClusterLabel] = clusterName

	return result, nil
}

var (
	// clusterDomainResolver is a function that resolves the cluster domain once and caches the result.
	// It's used to lazily resolve the cluster domain from the env var "TELEPORT_KUBE_CLUSTER_DOMAIN" or fallback to
	// a default value.
	// It's only used when agent is running in the Kubernetes cluster.
	clusterDomainResolver = sync.OnceValue[string](getClusterDomain)
)

const (
	// teleportKubeClusterDomain is the environment variable that specifies the cluster domain.
	teleportKubeClusterDomain = "TELEPORT_KUBE_CLUSTER_DOMAIN"
)

func getClusterDomain() string {
	if envDomain := os.Getenv(teleportKubeClusterDomain); envDomain != "" {
		return envDomain
	}
	return "cluster.local"
}

// RewriteHeadersAndApplyValueTraits rewrites the provided request's headers
// while applying value traits to them.
func RewriteHeadersAndApplyValueTraits(r *http.Request, rewrites iter.Seq[*types.Header], rewriteTraits wrappers.Traits, log *slog.Logger) {
	for header := range rewrites {
		values, err := ApplyValueTraits(header.Value, rewriteTraits)
		if err != nil {
			log.DebugContext(r.Context(), "Failed to apply traits",
				"header_value", header.Value,
				"error", err,
			)
			continue
		}
		r.Header.Del(header.Name)
		for _, value := range values {
			switch http.CanonicalHeaderKey(header.Name) {
			case teleport.HostHeader:
				r.Host = value
			default:
				r.Header.Add(header.Name, value)
			}
		}
	}
}
