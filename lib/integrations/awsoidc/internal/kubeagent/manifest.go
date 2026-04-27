// Teleport
// Copyright (C) 2026 Gravitational, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// Package kubeagent produces the Kubernetes objects that install the
// teleport-kube-agent Helm chart, without depending on Helm at runtime.
//
// The chart is rendered at build time by internal/gen into zz_generated.go
// as typed Go constructors. Manifests composes those constructors based on
// Options, applying the runtime toggles (enterprise / updater / HA) that
// Helm expresses as template conditionals, and overriding chart-rendered
// placeholder values with the runtime Options.
package kubeagent

//go:generate go run -C ./internal/gen . -chart ../../../../../../../examples/chart/teleport-kube-agent -values ../../testdata/values.yaml -out ../../zz_generated.go

import (
	"strings"

	"github.com/gravitational/trace"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Role names the set of Teleport services the agent runs.
type Role string

const (
	// RoleKube indicates only the Kubernetes role is enabled.
	RoleKube Role = "kube"
	// RoleKubeAppDiscovery indicates the Kubernetes, App and Discovery roles are enabled.
	RoleKubeAppDiscovery Role = "kube,app,discovery"
)

// The placeholders below match the values.yaml placeholders used when
// rendering the chart. These values are replaced at runtime based on
// the Options provided to Manifests.
const (
	placeholderNamespace = "ns-placeholder"
	placeholderProxy     = "proxy-placeholder:443"
	placeholderToken     = "token-placeholder"
	placeholderCluster   = "cluster-placeholder"
	placeholderChannel   = "channel-placeholder"
)

// Options configure the teleport-kube-agent chart.
type Options struct {
	Namespace       string
	ProxyAddr       string
	AuthToken       string
	KubeClusterName string
	Roles           Role

	Enterprise bool

	Updater        bool
	UpdaterChannel string

	HighAvailability bool

	AgentImage string

	Labels map[string]string
}

func (o Options) validate() error {
	switch {
	case o.Namespace == "":
		return trace.BadParameter("Namespace is required")
	case o.ProxyAddr == "":
		return trace.BadParameter("ProxyAddr is required")
	case o.AuthToken == "":
		return trace.BadParameter("AuthToken is required")
	case o.KubeClusterName == "":
		return trace.BadParameter("KubeClusterName is required")
	case o.Roles != RoleKube && o.Roles != RoleKubeAppDiscovery:
		return trace.BadParameter("Roles must be %q or %q, got %q", RoleKube, RoleKubeAppDiscovery, o.Roles)
	case o.Updater && o.UpdaterChannel == "":
		return trace.BadParameter("UpdaterChannel is required when Updater is true")
	}
	return nil
}

// Manifests returns the set of Kubernetes objects derived from the
// teleport-kube-agent with opts applied.
func Manifests(opts Options) ([]client.Object, error) {
	if err := opts.validate(); err != nil {
		return nil, trace.Wrap(err)
	}

	sa := genServiceAccount(opts)
	role := genRole(opts)
	rb := genRoleBinding(opts)
	cr := genClusterRole(opts)
	crb := genClusterRoleBinding(opts)
	cm := genConfigMap(opts)
	sec := genSecret(opts)
	sts := genStatefulSet(opts)

	objs := []client.Object{sa, role, rb, cr, crb, cm, sec, sts}

	if opts.HighAvailability {
		objs = append(objs, genPodDisruptionBudget(opts))
	}

	var updaterRoleBinding *rbacv1.RoleBinding
	var updaterDeployment *appsv1.Deployment
	if opts.Updater {
		updaterRoleBinding = genUpdaterRoleBinding(opts)
		updaterDeployment = genUpdaterDeployment(opts)
		objs = append(objs,
			genUpdaterServiceAccount(opts),
			genUpdaterRole(opts),
			updaterRoleBinding,
			updaterDeployment,
		)
	}

	// Override the namespace placeholder.
	for _, o := range objs {
		o.SetNamespace(opts.Namespace)
	}
	setSubjectNamespace(rb.Subjects, opts.Namespace)
	setSubjectNamespace(crb.Subjects, opts.Namespace)
	if updaterRoleBinding != nil {
		setSubjectNamespace(updaterRoleBinding.Subjects, opts.Namespace)
	}

	// Override the auth token placeholder.
	sec.StringData["auth-token"] = opts.AuthToken

	// Override the teleport config.
	cm.Data = map[string]string{"teleport.yaml": applyConfigSubs(teleportConfig(opts.Roles), opts)}

	// Override the agent's container image. AgentImage takes precedence,
	// otherwise the chart's built-in image is flipped to OSS when Enterprise is false.
	// The updater container is left at the chart-baked image.
	c := &sts.Spec.Template.Spec.Containers[0]
	switch {
	case opts.AgentImage != "":
		c.Image = opts.AgentImage
		// The chart embeds the version in env vars (e.g. TELEPORT_EXT_UPGRADER_VERSION)
		// for self-reporting; rewrite those to match the override's tag.
		newTag := opts.AgentImage
		if i := strings.LastIndex(opts.AgentImage, ":"); i >= 0 {
			newTag = opts.AgentImage[i+1:]
		}
		for i := range c.Env {
			c.Env[i].Value = strings.ReplaceAll(c.Env[i].Value, builtinImageTag, newTag)
		}
	case !opts.Enterprise:
		c.Image = strings.ReplaceAll(c.Image, "teleport-ent-distroless", "teleport-distroless")
	}

	// Use the specified number of replicas.
	replicas := int32(1)
	if opts.HighAvailability {
		replicas = 2
	}
	sts.Spec.Replicas = &replicas

	// Apply the provided labels.
	for k, v := range opts.Labels {
		sts.Spec.Template.Labels[k] = v
	}

	// Override the updater placeholders.
	if updaterDeployment != nil {
		customizeUpdaterArgs(updaterDeployment, opts)
	}

	return objs, nil
}

// teleportConfig selects the inner teleport.yaml variant for the given
// roles. The generator emits both variants as constants.
func teleportConfig(r Role) string {
	if r == RoleKubeAppDiscovery {
		return teleportConfigKubeAppDiscovery
	}
	return teleportConfigKube
}

// applyConfigSubs substitutes the placeholder values inside the rendered
// teleport.yaml payload. Used for fields that land inside an opaque string
// where Go's type system can't reach them.
func applyConfigSubs(payload string, opts Options) string {
	return strings.NewReplacer(
		placeholderProxy, opts.ProxyAddr,
		placeholderToken, opts.AuthToken,
		placeholderCluster, opts.KubeClusterName,
		placeholderNamespace, opts.Namespace,
	).Replace(payload)
}

// setSubjectNamespace overwrites the Namespace of every ServiceAccount
// subject in subs.
func setSubjectNamespace(subs []rbacv1.Subject, ns string) {
	for i := range subs {
		if subs[i].Kind == "ServiceAccount" {
			subs[i].Namespace = ns
		}
	}
}

// customizeUpdaterArgs walks the updater container's args and replaces
// every placeholder string with the corresponding runtime value. Also swaps
// the --base-image arg from the enterprise repo to OSS when
// opts.Enterprise is false, mirroring the StatefulSet image swap.
func customizeUpdaterArgs(d *appsv1.Deployment, opts Options) {
	replacer := strings.NewReplacer(
		placeholderProxy, opts.ProxyAddr,
		placeholderNamespace, opts.Namespace,
		placeholderChannel, opts.UpdaterChannel,
	)
	args := d.Spec.Template.Spec.Containers[0].Args
	for i, a := range args {
		a = replacer.Replace(a)
		if !opts.Enterprise {
			a = strings.ReplaceAll(a, "teleport-ent-distroless", "teleport-distroless")
		}
		args[i] = a
	}
}

// ptr returns a pointer to v. Used by code generation for pointer types (*int32).
func ptr[T any](v T) *T { return &v }
