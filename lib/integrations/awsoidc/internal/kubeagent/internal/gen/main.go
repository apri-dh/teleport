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

// Command gen renders the teleport-kube-agent Helm chart and emits Go source
// with typed constructors for every rendered Kubernetes resource.
//
// The chart is rendered in-process via the helm.sh/helm/v3 Go SDK. This
// command is in a separate Go module to keep the dependencies isolated
// from the main module to enable dead code elimination.
//
// It is meant to be invoked via go:generate from kubeagent/manifest.go.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"slices"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/getter"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	fs := flag.NewFlagSet("kubeagent-gen", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	chartDir := fs.String("chart", "", "path to the teleport-kube-agent chart directory")
	valuesFile := fs.String("values", "", "path to the values file to render with")
	outFile := fs.String("out", "zz_generated.go", "output file")

	if err := fs.Parse(arguments); err != nil {
		return fmt.Errorf("parsing arguments: %w", err)
	}

	if *chartDir == "" || *valuesFile == "" {
		return errors.New("both -chart and -values are required")
	}

	// Load the chart once and reuse it below.
	ch, err := loader.Load(*chartDir)
	if err != nil {
		return fmt.Errorf("loading chart from %q: %w", *chartDir, err)
	}

	// Primary render of the helm chart with the provided values and roles=kube.
	renderedObjects, err := renderHelmChart(ch, *valuesFile, "kube")
	if err != nil {
		return fmt.Errorf("render (roles=kube): %w", err)
	}

	// Secondary render of just the ConfigMap template with
	// roles=kube,app,discovery so the alternate teleport.yaml payload can be
	// embedded as a constant and selected at runtime.
	renderedConfigWithAllRoles, err := renderConfigMap(ch, *valuesFile, "kube", "app", "discovery")
	if err != nil {
		return fmt.Errorf("render alt config (roles=kube,app,discovery): %w", err)
	}

	var buf bytes.Buffer
	if err := generateSourceCode(&buf, renderedObjects, renderedConfigWithAllRoles); err != nil {
		return fmt.Errorf("emit: %w", err)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		// Write unformatted output to a sibling file to make debugging easy.
		debugPath := *outFile + ".debug"
		_ = os.WriteFile(debugPath, buf.Bytes(), 0o644)
		return fmt.Errorf("gofmt failed: %w (unformatted output written to %s)", err, debugPath)
	}

	if err := os.WriteFile(*outFile, formatted, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *outFile, err)
	}

	return nil
}

// resourceID uniquely identifies a rendered Kubernetes object.
type resourceID struct {
	Kind string
	Name string
}

// compare orders resources by Kind, then Name.
func (a resourceID) compare(b resourceID) int {
	if c := strings.Compare(a.Kind, b.Kind); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

// renderHelmChart renders every template in the helm chart and returns the decoded
// objects keyed by {Kind, Name}. Any provided roles are applied via the equivalent
// of --set roles=value.
func renderHelmChart(ch *chart.Chart, valuesFile string, roles ...string) (map[resourceID]runtime.Object, error) {
	rendered, err := renderTemplates(ch, valuesFile, roles)
	if err != nil {
		return nil, err
	}

	out := map[resourceID]runtime.Object{}
	for path, content := range rendered {
		if strings.TrimSpace(content) == "" ||
			!strings.Contains(path, "/templates/") ||
			!strings.HasSuffix(path, ".yaml") &&
				!strings.HasSuffix(path, ".yml") {
			continue
		}

		objs, err := decodeObjects(content)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}

		for _, obj := range objs {
			var id resourceID
			if gvks, _, err := scheme.Scheme.ObjectKinds(obj); err == nil && len(gvks) > 0 {
				id.Kind = gvks[0].Kind
			}
			if mo, ok := obj.(interface{ GetName() string }); ok {
				id.Name = mo.GetName()
			}

			if _, exists := out[id]; exists {
				return nil, fmt.Errorf("duplicate resource %s/%s (rendered by %s)", id.Kind, id.Name, path)
			}
			out[id] = obj
		}
	}
	return out, nil
}

// renderConfigMap renders the ConfigMap template for the given
// roles and returns the inner teleport.yaml string.
func renderConfigMap(ch *chart.Chart, valuesFile string, roles ...string) (string, error) {
	rendered, err := renderTemplates(ch, valuesFile, roles)
	if err != nil {
		return "", err
	}

	for path, content := range rendered {
		if !strings.HasSuffix(path, "/templates/config.yaml") {
			continue
		}

		objs, err := decodeObjects(content)
		if err != nil {
			return "", fmt.Errorf("decode %s: %w", path, err)
		}

		for _, obj := range objs {
			cm, ok := obj.(*corev1.ConfigMap)
			if !ok {
				continue
			}
			payload := cm.Data["teleport.yaml"]
			if payload == "" {
				return "", errors.New("teleport.yaml empty in rendered config.yaml")
			}
			return payload, nil
		}

		return "", errors.New("ConfigMap not found in rendered config.yaml")
	}

	return "", errors.New("templates/config.yaml not present in rendered output")
}

// renderTemplates runs the helm rendering engine in-process and returns its
// raw map of template path to rendered content.
func renderTemplates(ch *chart.Chart, valuesFile string, roles []string) (map[string]string, error) {
	valOpts := &values.Options{ValueFiles: []string{valuesFile}}
	if len(roles) > 0 {
		valOpts.Values = []string{"roles=" + strings.Join(roles, `\,`)}
	}

	vals, err := valOpts.MergeValues(getter.All(cli.New()))
	if err != nil {
		return nil, fmt.Errorf("merging values: %w", err)
	}

	const (
		// releaseName is the helm release name passed to the engine
		releaseName = "teleport-kube-agent"

		// placeholderNamespace is the release namespace passed to the helm engine
		placeholderNamespace = "ns-placeholder"
	)

	relOpts := chartutil.ReleaseOptions{
		Name:      releaseName,
		Namespace: placeholderNamespace,
		Revision:  1,
		IsInstall: true,
	}

	rv, err := chartutil.ToRenderValues(ch, vals, relOpts, chartutil.DefaultCapabilities)
	if err != nil {
		return nil, fmt.Errorf("preparing render values: %w", err)
	}

	return engine.Render(ch, rv)
}

// decodeObjects parses a rendered template's content into the typed
// runtime.Objects it contains. Multi-doc inputs are returned in
// document order. Empty or whitespace-only docs are skipped.
func decodeObjects(content string) ([]runtime.Object, error) {
	decoder := scheme.Codecs.UniversalDeserializer()
	reader := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(content)))
	var out []runtime.Object
	for i := 0; ; i++ {
		doc, err := reader.Read()
		switch {
		case errors.Is(err, io.EOF):
			return out, nil
		case err != nil:
			return nil, fmt.Errorf("doc %d read: %w", i, err)
		case len(bytes.TrimSpace(doc)) == 0:
			continue
		}

		obj, _, err := decoder.Decode(doc, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("doc %d: %w", i, err)
		}
		out = append(out, obj)
	}
}

// generateSourceCode creates and writes the Go source from the rendered Kubernetes
// resources to w. Resources are emitted in (Kind, Name) order so that the output is
// always deterministic.
func generateSourceCode(w io.Writer, renderedObjects map[resourceID]runtime.Object, renderedConfigWithAllRoles string) error {
	var body strings.Builder
	usedAliases := map[string]bool{}

	kubeOnlyTeleportConfig := extractTeleportConfig(renderedObjects)
	if kubeOnlyTeleportConfig == "" {
		return errors.New("no teleport.yaml found in ConfigMap generated with roles=kube")
	}
	if renderedConfigWithAllRoles == "" {
		return errors.New("no teleport.yaml found in ConfigMap generated with roles=app,discovery,kube")
	}

	builtinImageTag := extractBuiltinImageTag(renderedObjects)
	if builtinImageTag == "" {
		return errors.New("could not determine the chart's built-in image tag from the rendered StatefulSet")
	}

	fmt.Fprintln(&body, "// Potential teleport.yaml payloads. Picked at runtime based on the specified roles.")
	fmt.Fprintf(&body, "const teleportConfigKube = %s\n\n", backtickQuote(kubeOnlyTeleportConfig))
	fmt.Fprintf(&body, "const teleportConfigKubeAppDiscovery = %s\n\n", backtickQuote(renderedConfigWithAllRoles))

	fmt.Fprintln(&body, "// builtinImageTag is the chart's image tag at build time. The composer")
	fmt.Fprintln(&body, "// uses it as the version search target when an Options.AgentImage override")
	fmt.Fprintln(&body, "// is supplied, so embedded version strings (e.g. TELEPORT_EXT_UPGRADER_VERSION)")
	fmt.Fprintln(&body, "// match the new tag.")
	fmt.Fprintf(&body, "const builtinImageTag = %q\n\n", builtinImageTag)

	ids := make([]resourceID, 0, len(renderedObjects))
	for id := range renderedObjects {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, resourceID.compare)

	for _, id := range ids {
		if err := emitConstructor(&body, id, renderedObjects[id], usedAliases); err != nil {
			return fmt.Errorf("emit %s/%s: %w", id.Kind, id.Name, err)
		}
	}

	fmt.Fprintln(w, "// Code generated by kubeagent/internal/gen. DO NOT EDIT.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "package kubeagent")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "import (")
	// Iterate packageAliases in sorted path order so the pre-gofmt buffer
	// (written to zz_generated.go.debug on gofmt failure) is deterministic.
	// go/format.Source will re-sort the imports in the final output.
	paths := make([]string, 0, len(packageAliases))
	for path := range packageAliases {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		alias := packageAliases[path]
		if !usedAliases[alias] {
			continue
		}
		fmt.Fprintf(w, "\t%s %q\n", alias, path)
	}
	fmt.Fprintln(w, ")")
	fmt.Fprintln(w)
	_, err := io.WriteString(w, body.String())
	return err
}

// emitConstructor writes a `func genFoo(opts Options) *Type { return &Type{...} }`
// declaration for obj to w.
func emitConstructor(w io.Writer, id resourceID, obj runtime.Object, used map[string]bool) error {
	var constructorName string
	switch {
	case strings.HasSuffix(id.Name, "-updater"):
		constructorName = "genUpdater" + id.Kind
	case id.Kind == "Secret" && id.Name == "teleport-kube-agent-join-token":
		constructorName = "genSecret"
	default:
		constructorName = "gen" + id.Kind
	}

	if cm, ok := obj.(*corev1.ConfigMap); ok {
		// The data is populated at runtime based on the Teleport roles.
		obj = cm.DeepCopy()
		obj.(*corev1.ConfigMap).Data = nil
	}

	p := newPrinter(used)
	p.writeRootPointer(obj)

	fmt.Fprintf(w, "func %s(opts Options) *%s {\n", constructorName, p.rootTypeName)
	fmt.Fprintf(w, "\treturn %s\n", p.String())
	fmt.Fprintf(w, "}\n\n")
	return nil
}

// extractTeleportConfig returns the teleport.yaml contents specified
// in the ConfigMap.
func extractTeleportConfig(objects map[resourceID]runtime.Object) string {
	obj, ok := objects[resourceID{Kind: "ConfigMap", Name: "teleport-kube-agent"}]
	if !ok {
		return ""
	}
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return ""
	}
	return cm.Data["teleport.yaml"]
}

// extractBuiltinImageTag returns the chart's image tag from the container image.
func extractBuiltinImageTag(objects map[resourceID]runtime.Object) string {
	obj, ok := objects[resourceID{Kind: "StatefulSet", Name: "teleport-kube-agent"}]
	if !ok {
		return ""
	}
	sts, ok := obj.(*appsv1.StatefulSet)
	if !ok || len(sts.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	image := sts.Spec.Template.Spec.Containers[0].Image
	i := strings.LastIndex(image, ":")
	if i < 0 {
		return ""
	}
	return image[i+1:]
}

// backtickQuote wraps s in backticks for embedding as a Go raw string literal.
// Falls back to strconv.Quote (via %q) if s contains a backtick.
func backtickQuote(s string) string {
	if strings.ContainsRune(s, '`') {
		return fmt.Sprintf("%q", s)
	}
	return "`" + s + "`"
}
