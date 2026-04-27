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

package kubeagent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/gravitational/teleport/lib/config"
)

func baseOpts() Options {
	return Options{
		Namespace:       "teleport-agent",
		ProxyAddr:       "example.teleport.sh:443",
		AuthToken:       "join-token-value",
		KubeClusterName: "my-eks-cluster",
		Roles:           RoleKube,
	}
}

func TestManifests_MinimalOSS(t *testing.T) {
	t.Parallel()

	opts := baseOpts()
	objs, err := Manifests(opts)
	require.NoError(t, err)

	kinds := kindsByType(objs)
	require.Equal(t, 1, kinds["StatefulSet"])
	require.Equal(t, 0, kinds["Deployment"])
	require.Equal(t, 0, kinds["PodDisruptionBudget"])
	require.Equal(t, 1, kinds["ServiceAccount"])
	require.Equal(t, 1, kinds["Role"])
	require.Equal(t, 1, kinds["RoleBinding"])
	require.Equal(t, 1, kinds["ClusterRole"])
	require.Equal(t, 1, kinds["ClusterRoleBinding"])
	require.Equal(t, 1, kinds["ConfigMap"])
	require.Equal(t, 1, kinds["Secret"])

	sts := getObject[*appsv1.StatefulSet](t, objs)
	require.EqualValues(t, 1, *sts.Spec.Replicas)
	require.Contains(t, sts.Spec.Template.Spec.Containers[0].Image, "teleport-distroless:")
	require.NotContains(t, sts.Spec.Template.Spec.Containers[0].Image, "teleport-ent-distroless:")

	cm := getObject[*corev1.ConfigMap](t, objs)
	require.Equal(t, baseOpts().Namespace, cm.Namespace)

	cfg, err := config.ReadConfig(strings.NewReader(cm.Data["teleport.yaml"]))
	require.NoError(t, err)

	require.Equal(t, opts.KubeClusterName, cfg.Kube.KubeClusterName)
	require.True(t, cfg.Kube.Enabled())
	require.False(t, cfg.Apps.Enabled())
}

func TestManifests_EnterpriseImage(t *testing.T) {
	t.Parallel()

	opts := baseOpts()
	opts.Enterprise = true

	objs, err := Manifests(opts)
	require.NoError(t, err)

	sts := getObject[*appsv1.StatefulSet](t, objs)
	require.Contains(t, sts.Spec.Template.Spec.Containers[0].Image, "teleport-ent-distroless:")
}

func TestManifests_AgentImage(t *testing.T) {
	t.Parallel()

	const override = "public.ecr.example.com/gravitational-staging/teleport-distroless:99.0.0-alpha.1"
	const overrideTag = "99.0.0-alpha.1"

	opts := baseOpts()
	opts.Enterprise = false // ensure the OSS swap branch is the alternative
	opts.Updater = true     // forces TELEPORT_EXT_UPGRADER_VERSION to be present
	opts.UpdaterChannel = "stable/cloud"
	opts.AgentImage = override

	objs, err := Manifests(opts)
	require.NoError(t, err)

	sts := getObject[*appsv1.StatefulSet](t, objs)
	c := sts.Spec.Template.Spec.Containers[0]

	// Image taken verbatim, not subjected to the OSS substring swap.
	require.Equal(t, override, c.Image)

	// Env var carrying the version is rewritten to the override's tag.
	var found bool
	for _, e := range c.Env {
		if e.Name == "TELEPORT_EXT_UPGRADER_VERSION" {
			require.Equal(t, overrideTag, e.Value)
			found = true
			break
		}
	}
	require.True(t, found, "TELEPORT_EXT_UPGRADER_VERSION env var not present (should be when Updater=true)")
}

func TestManifests_HighAvailability(t *testing.T) {
	t.Parallel()

	opts := baseOpts()
	opts.HighAvailability = true

	objs, err := Manifests(opts)
	require.NoError(t, err)

	sts := getObject[*appsv1.StatefulSet](t, objs)
	require.EqualValues(t, 2, *sts.Spec.Replicas)

	pdb := getObject[*policyv1.PodDisruptionBudget](t, objs)
	require.NotNil(t, pdb.Spec.MinAvailable)
	require.Equal(t, 1, pdb.Spec.MinAvailable.IntValue())
}

func TestManifests_Updater(t *testing.T) {
	t.Parallel()

	opts := baseOpts()
	opts.Updater = true
	opts.UpdaterChannel = "stable/cloud"

	objs, err := Manifests(opts)
	require.NoError(t, err)

	kinds := kindsByType(objs)
	require.Equal(t, 1, kinds["Deployment"])
	require.Equal(t, 2, kinds["ServiceAccount"])
	require.Equal(t, 2, kinds["Role"])
	require.Equal(t, 2, kinds["RoleBinding"])
}

func TestManifests_KubeAppDisc_ConfigPayload(t *testing.T) {
	t.Parallel()

	opts := baseOpts()
	opts.Roles = RoleKubeAppDiscovery

	objs, err := Manifests(opts)
	require.NoError(t, err)

	cm := getObject[*corev1.ConfigMap](t, objs)
	cfg, err := config.ReadConfig(strings.NewReader(cm.Data["teleport.yaml"]))
	require.NoError(t, err)
	require.True(t, cfg.Discovery.Enabled())
	require.True(t, cfg.Apps.Enabled())
	require.True(t, cfg.Kube.Enabled())
	require.False(t, cfg.Auth.Enabled())
	require.False(t, cfg.Databases.Enabled())

	kubeOnly, err := Manifests(baseOpts())
	require.NoError(t, err)
	kubeCM := getObject[*corev1.ConfigMap](t, kubeOnly)
	cfg, err = config.ReadConfig(strings.NewReader(kubeCM.Data["teleport.yaml"]))
	require.NoError(t, err)
	require.True(t, cfg.Kube.Enabled())
	require.False(t, cfg.Apps.Enabled())
	require.False(t, cfg.Discovery.Enabled())
	require.False(t, cfg.Auth.Enabled())
	require.False(t, cfg.Databases.Enabled())
}

func TestManifests_Labels(t *testing.T) {
	t.Parallel()

	opts := baseOpts()
	opts.Labels = map[string]string{
		"teleport.internal/resource-id": "abc-123",
		"region":                        "us-east-1",
	}

	objs, err := Manifests(opts)
	require.NoError(t, err)

	sts := getObject[*appsv1.StatefulSet](t, objs)
	for k, v := range opts.Labels {
		require.Equal(t, v, sts.Spec.Template.Labels[k], "label %s missing or wrong on pod template", k)
	}
}

func TestManifests_NoPlaceholderLeaks(t *testing.T) {
	t.Parallel()

	opts := baseOpts()
	opts.Enterprise = true
	opts.HighAvailability = true
	opts.Updater = true
	opts.UpdaterChannel = "stable/cloud"
	opts.Labels = map[string]string{"region": "us-east-1"}

	objs, err := Manifests(opts)
	require.NoError(t, err)

	placeholders := []string{
		placeholderNamespace,
		placeholderProxy,
		placeholderToken,
		placeholderCluster,
		placeholderChannel,
	}
	for _, o := range objs {
		yaml, err := sigsyaml.Marshal(o)
		require.NoError(t, err)
		for _, p := range placeholders {
			require.NotContainsf(t, string(yaml), p, "placeholder %q leaked into %T %s/%s", p, o, o.GetNamespace(), o.GetName())
		}
	}
}

func kindsByType(objs []client.Object) map[string]int {
	out := map[string]int{}
	for _, o := range objs {
		out[reflect.TypeOf(o).Elem().Name()]++
	}
	return out
}

// getObject finds exactly one object of type T in objs, failing the test if
// there are zero or more than one matches.
func getObject[T client.Object](t *testing.T, objs []client.Object) T {
	t.Helper()
	var matches []T
	for _, o := range objs {
		if cast, ok := o.(T); ok {
			matches = append(matches, cast)
		}
	}
	require.Len(t, matches, 1, "expected exactly one %T", *new(T))
	return matches[0]
}
