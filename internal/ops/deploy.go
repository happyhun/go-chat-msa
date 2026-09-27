package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

func (a *app) kindUp(ctx context.Context) error {
	clusters, err := a.runner.output(ctx, "kind", "get", "clusters")
	if err != nil {
		return err
	}
	if slices.Contains(strings.Fields(string(clusters)), a.cluster) {
		a.log("Using existing cluster: %s", a.cluster)
	} else {
		a.log("Creating cluster: %s (first setup may take a few minutes)", a.cluster)
		if err := a.runner.run(ctx, "kind", "create", "cluster", "--name", a.cluster, "--config", a.kindConfig); err != nil {
			return err
		}
	}
	if err := a.platform(ctx); err != nil {
		return err
	}
	a.log("Applying node network settings and waiting for readiness")
	nodes, err := a.runner.output(ctx, "kind", "get", "nodes", "--name", a.cluster)
	if err != nil {
		return err
	}
	for node := range strings.FieldsSeq(string(nodes)) {
		for _, setting := range []string{"net.core.somaxconn=65535", "net.ipv4.ip_local_port_range=10240 65535"} {
			if _, err := a.runner.output(ctx, "docker", "exec", node, "sysctl", "-w", setting); err != nil {
				return err
			}
		}
	}
	if err := a.kubectl(ctx, "wait", "--for=condition=Ready", "nodes", "--all", "--timeout="+a.timeout.String()); err != nil {
		return err
	}
	a.log("Cluster ready: %s", a.cluster)
	return nil
}

func (a *app) platform(ctx context.Context) error {
	a.log("Platform: preparing the Traefik chart")
	if err := a.prepareDependencies(ctx, "platform"); err != nil {
		return err
	}
	if _, err := a.kubeOutput(ctx, "-n", "ingress-nginx", "get", "deployment", "ingress-nginx-controller"); err == nil {
		return errors.New("this cluster still uses ingress-nginx; use a new KIND_CLUSTER and free local ports")
	}
	a.log("Platform: waiting for nodes and cluster networking")
	if err := a.kubectl(ctx, "-n", "kube-system", "rollout", "status", "daemonset/kindnet", "--timeout=300s"); err != nil {
		return err
	}
	if err := a.kubectl(ctx, "wait", "--for=condition=Ready", "nodes", "--all", "--timeout=300s"); err != nil {
		return err
	}
	a.log("Platform: applying Gateway API resources")
	manifest, err := downloadVerified(ctx, "https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.1/standard-install.yaml", "24d931f22abd8e40c973264319ead7cfa09d0fb7716b7ab1ee2ff174cb063a73")
	if err != nil {
		return err
	}
	if err := a.apply(ctx, manifest, "--server-side"); err != nil {
		return err
	}
	if err := a.kubectl(ctx, "wait", "--for=condition=Established", "crd/gateways.gateway.networking.k8s.io", "crd/httproutes.gateway.networking.k8s.io", "--timeout=120s"); err != nil {
		return err
	}
	a.log("Platform: deploying Traefik and waiting for readiness")
	if err := a.runner.run(ctx, a.helm, "upgrade", "--install", "platform", filepath.Join(a.helmDir(), "charts", "platform"),
		"--kube-context", a.kubeContext(), "-n", "gochat-system", "--create-namespace", "--reset-values", "--skip-crds", "--wait=legacy", "--timeout", a.timeout.String(),
		"-f", filepath.Join(a.helmDir(), "values", "common", "platform.yaml")); err != nil {
		return err
	}
	if err := a.kubectl(ctx, "-n", "gochat-system", "rollout", "status", "deployment/traefik", "--timeout=180s"); err != nil {
		return err
	}
	a.log("Platform ready")
	return nil
}

func downloadVerified(ctx context.Context, url, checksum string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(data)) != checksum {
		return nil, fmt.Errorf("checksum mismatch for %s", url)
	}
	return data, nil
}

func (a *app) apply(ctx context.Context, manifest []byte, extra ...string) error {
	args := []string{"--context", a.kubeContext(), "apply", "-f", "-"}
	args = append(args, extra...)
	_, err := a.runner.call(ctx, invocation{name: "kubectl", args: args, input: manifest})
	return err
}

func fileChecksum(paths ...string) (string, error) {
	hash := sha256.New()
	for _, path := range paths {
		// #nosec G304 -- Paths are repository configuration files selected by the CLI.
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(hash, "%x  %s\n", sha256.Sum256(data), path)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func directoryChecksum(dir string) (string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	// Match the sorted shasum records used by the existing chart annotations.
	var records []string
	for _, path := range files {
		// #nosec G304 -- Paths are repository configuration files selected by the CLI.
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		records = append(records, fmt.Sprintf("%x  %s\n", sha256.Sum256(data), path))
	}
	slices.Sort(records)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(records, "")))), nil
}

func (a *app) dependenciesReady(ctx context.Context, chart string) bool {
	data, err := a.runner.output(ctx, a.helm, "dependency", "list", chart)
	if err != nil {
		return false
	}
	for i, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if i > 0 && len(fields) > 0 && fields[len(fields)-1] != "ok" {
			return false
		}
	}
	return true
}

func (a *app) prepareDependencies(ctx context.Context, groups ...string) error {
	reposReady := false
	for _, group := range groups {
		chart := filepath.Join(a.helmDir(), "charts", group)
		stamp := filepath.Join(chart, "charts", ".dependencies.sha256")
		checksum, err := fileChecksum(filepath.Join(chart, "Chart.yaml"), filepath.Join(chart, "Chart.lock"))
		if err != nil {
			return err
		}
		// #nosec G304 -- The dependency stamp is inside a fixed repository chart directory.
		stored, readErr := os.ReadFile(stamp)
		if readErr == nil && strings.TrimSpace(string(stored)) == checksum && a.dependenciesReady(ctx, chart) {
			continue
		}
		if !reposReady {
			for _, repo := range [][2]string{
				{"gochat-nats", "https://nats-io.github.io/k8s/helm/charts/"},
				{"gochat-traefik", "https://traefik.github.io/charts"},
				{"gochat-grafana", "https://grafana.github.io/helm-charts"},
				{"gochat-grafana-community", "https://grafana-community.github.io/helm-charts"},
				{"gochat-prometheus", "https://prometheus-community.github.io/helm-charts"},
			} {
				if err := a.runner.run(ctx, a.helm, "repo", "add", repo[0], repo[1]); err != nil {
					return err
				}
			}
			reposReady = true
		}
		if err := os.Remove(stamp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		a.log("%s: preparing chart dependencies", group)
		if err := a.runner.run(ctx, a.helm, "dependency", "build", chart); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(stamp), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(stamp, []byte(checksum+"\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) prepareCharts(ctx context.Context, extra ...string) error {
	if err := a.prepareDependencies(ctx, append([]string{"infra", "apps", "observability"}, extra...)...); err != nil {
		return err
	}
	stage, err := os.MkdirTemp("", "gochat-ops-")
	if err != nil {
		return err
	}
	a.stage = stage
	if err := os.CopyFS(stage, os.DirFS(filepath.Join(a.helmDir(), "charts"))); err != nil {
		return err
	}
	if err := os.CopyFS(filepath.Join(stage, "observability", "files"), os.DirFS(filepath.Join(a.root, "observability"))); err != nil {
		return err
	}
	for _, entry := range [][2]string{
		{"services/swagger-ui/files/openapi-spec", "api/openapi/openapi.yaml"},
		{"migrations/files/postgres-migrations", "db/migrations/postgres/*.sql"},
		{"migrations/files/mongo-migrations", "db/migrations/mongo/*.json"},
		{"load/files/k6-load-scripts", "test/load/*.js"},
	} {
		if err := a.copyFiles(entry[0], entry[1]); err != nil {
			return err
		}
	}
	for _, group := range []string{"infra", "apps", "observability"} {
		chart := filepath.Join(stage, group)
		// #nosec G304 -- The chart is copied from the repository into our temporary staging directory.
		data, err := os.ReadFile(filepath.Join(chart, "Chart.yaml"))
		if err != nil {
			return err
		}
		var metadata struct {
			Dependencies []struct {
				Repository string `yaml:"repository"`
			} `yaml:"dependencies"`
		}
		if err := yaml.Unmarshal(data, &metadata); err != nil {
			return err
		}
		for _, dep := range metadata.Dependencies {
			if local, ok := strings.CutPrefix(dep.Repository, "file://"); ok {
				if _, err := a.runner.output(ctx, a.helm, "package", filepath.Join(chart, local), "--destination", filepath.Join(chart, "charts")); err != nil {
					return err
				}
			}
		}
		if !a.dependenciesReady(ctx, chart) {
			return fmt.Errorf("invalid chart dependencies for %s", group)
		}
	}
	return nil
}

func (a *app) copyFiles(target, pattern string) error {
	files, err := filepath.Glob(filepath.Join(a.root, pattern))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no files match %s", pattern)
	}
	dir := filepath.Join(a.stage, target)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	destination, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = destination.Close() }()
	for _, file := range files {
		// #nosec G304 -- Files match fixed repository asset patterns.
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if err := destination.WriteFile(filepath.Base(file), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) valuesArgs(group string) ([]string, error) {
	args := []string{"-f", filepath.Join(a.helmDir(), "values", "common", group+".yaml"), "-f", filepath.Join(a.helmDir(), "values", a.environment, group+".yaml")}
	if group != "observability" {
		return args, nil
	}
	for _, entry := range [][2]string{{"loki.loki.config", "loki"}, {"tempo.config", "tempo"}, {"pyroscope.pyroscope.config", "pyroscope"}} {
		args = append(args, "--set-file", entry[0]+"="+filepath.Join(a.root, "observability", entry[1], "config.yaml"))
	}
	args = append(args, "--set", "alloy.rbac.namespaces[0]="+a.namespace())
	for _, entry := range [][2]string{{"prometheus", "prometheus.server"}, {"grafana", "grafana"}, {"alloy", "alloy.controller"}} {
		checksum, err := directoryChecksum(filepath.Join(a.root, "observability", entry[0]))
		if err != nil {
			return nil, err
		}
		args = append(args, "--set-string", entry[1]+".podAnnotations.checksum/config="+checksum)
	}
	return args, nil
}

func (a *app) upgrade(ctx context.Context, group string, extra ...string) error {
	values, err := a.valuesArgs(group)
	if err != nil {
		return err
	}
	args := []string{"upgrade", "--install", group, filepath.Join(a.stage, group), "--kube-context", a.kubeContext(), "-n", a.namespace(), "--create-namespace", "--reset-values", "--wait=legacy", "--timeout", a.timeout.String()}
	return a.runner.run(ctx, a.helm, append(append(args, values...), extra...)...)
}

func (a *app) render(ctx context.Context, group string, extra ...string) ([]byte, error) {
	values, err := a.valuesArgs(group)
	if err != nil {
		return nil, err
	}
	args := []string{"template", group, filepath.Join(a.stage, group), "-n", a.namespace()}
	return a.runner.output(ctx, a.helm, append(append(args, values...), extra...)...)
}

func (a *app) uninstall(ctx context.Context, release string, timeout time.Duration) error {
	return a.runner.run(ctx, a.helm, "uninstall", release, "--kube-context", a.kubeContext(), "-n", a.namespace(), "--ignore-not-found", "--wait", "--timeout", timeout.String())
}

func renderedImages(manifest []byte) ([]string, error) {
	var images []string
	var visit func(*yaml.Node)
	visit = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == "image" && n.Content[i+1].Kind == yaml.ScalarNode {
					images = append(images, n.Content[i+1].Value)
				}
			}
		}
		for _, child := range n.Content {
			visit(child)
		}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(manifest))
	for {
		var doc yaml.Node
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return images, nil
			}
			return nil, err
		}
		visit(&doc)
	}
}

func (a *app) buildImages(ctx context.Context) error {
	manifest, err := a.render(ctx, "apps")
	if err != nil {
		return err
	}
	images, err := renderedImages(manifest)
	if err != nil {
		return err
	}
	var overrides strings.Builder
	for _, service := range []string{"api-gateway", "websocket-service", "user-service", "chat-service", "frontend"} {
		var matches []string
		for _, image := range images {
			if strings.HasPrefix(image, "go-chat-msa/"+service+":") {
				matches = append(matches, image)
			}
		}
		if len(matches) != 1 {
			return fmt.Errorf("expected exactly one image for %s, found %d", service, len(matches))
		}
		image := matches[0]
		a.log("%s: building image (unchanged steps use the build cache)", service)
		args := []string{"build", "--pull", "--provenance=false", "-t", image}
		if service == "frontend" {
			args = append(args, filepath.Join(a.root, "frontend"))
		} else {
			args = append(args, "--build-arg", "SERVICE_NAME="+service, a.root)
		}
		if err := a.runner.run(ctx, "docker", args...); err != nil {
			return err
		}
		id, err := a.runner.output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image)
		if err != nil {
			return err
		}
		digest := strings.TrimSpace(string(id))
		if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
			return fmt.Errorf("invalid image ID %q", digest)
		}
		tag := "sha256-" + strings.TrimPrefix(digest, "sha256:")
		tagged := image[:strings.LastIndex(image, ":")] + ":" + tag
		if err := a.runner.run(ctx, "docker", "tag", image, tagged); err != nil {
			return err
		}
		a.log("%s: loading image into the cluster", service)
		if err := a.runner.run(ctx, "kind", "load", "docker-image", "--name", a.cluster, tagged); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(&overrides, "%s:\n  image:\n    tag: %s\n", service, tag)
	}
	return os.WriteFile(filepath.Join(a.stage, "images.yaml"), []byte(overrides.String()), 0o600)
}

func (a *app) up(ctx context.Context) error {
	if _, err := a.kubeOutput(ctx, "-n", a.namespace(), "get", "deployment/postgres"); err == nil {
		if _, err := a.runner.output(ctx, a.helm, "status", "infra", "--kube-context", a.kubeContext(), "-n", a.namespace()); err != nil {
			return fmt.Errorf("unmanaged resources exist in %s; use a new kind cluster for Helm migration", a.namespace())
		}
	}
	a.log("%s [1/6]: preparing charts", a.environment)
	if err := a.prepareCharts(ctx); err != nil {
		return err
	}
	a.log("%s [2/6]: building and loading images", a.environment)
	if err := a.buildImages(ctx); err != nil {
		return err
	}
	namespace, err := a.kubeOutput(ctx, "create", "namespace", a.namespace(), "--dry-run=client", "-o", "yaml")
	if err != nil {
		return err
	}
	if err := a.apply(ctx, namespace); err != nil {
		return err
	}
	labels := []string{"label", "namespace", a.namespace(), "--overwrite", "app.kubernetes.io/name=" + a.namespace(), "app.kubernetes.io/instance=" + a.namespace(), "app.kubernetes.io/component=namespace", "app.kubernetes.io/part-of=go-chat-msa"}
	if a.environment != "qa" {
		labels = append(labels, "pod-security.kubernetes.io/audit=restricted", "pod-security.kubernetes.io/audit-version=v1.37")
	}
	if err := a.kubectl(ctx, labels...); err != nil {
		return err
	}
	a.log("%s [3/6]: deploying databases, Redis and NATS; waiting for readiness", a.environment)
	if err := a.upgrade(ctx, "infra"); err != nil {
		return err
	}
	a.log("%s [4/6]: deploying observability services; waiting for readiness", a.environment)
	if err := a.upgrade(ctx, "observability"); err != nil {
		return err
	}
	if a.environment == "qa" {
		if err := a.kubectl(ctx, "wait", "--for=condition=Available", "apiservice/v1beta1.custom.metrics.k8s.io", "--timeout="+a.timeout.String()); err != nil {
			return err
		}
	}
	a.log("%s [5/6]: running database migrations; waiting for completion", a.environment)
	if err := a.uninstall(ctx, "migrations", a.timeout); err != nil {
		return err
	}
	if err := a.upgrade(ctx, "migrations", "--wait=watcher", "--wait-for-jobs"); err != nil {
		for _, job := range []string{"postgres-migrate", "mongo-migrate"} {
			_ = a.kubectl(ctx, "--request-timeout=5s", "-n", a.namespace(), "describe", "job/"+job)
			_ = a.kubectl(ctx, "--request-timeout=5s", "-n", a.namespace(), "logs", "job/"+job, "--all-containers=true", "--tail=100", "--pod-running-timeout=5s")
		}
		return err
	}
	a.log("%s [6/6]: deploying apps; waiting for apps and gateway readiness", a.environment)
	if err := a.upgrade(ctx, "apps", "-f", filepath.Join(a.stage, "images.yaml")); err != nil {
		return err
	}
	if err := a.kubectl(ctx, "-n", a.namespace(), "wait", "--for=condition=Programmed", "gateway/gochat", "--timeout="+a.timeout.String()); err != nil {
		return err
	}
	a.log("%s ready: http://%s.gochat.localhost:30080/", a.environment, a.environment)
	return nil
}

func (a *app) down(ctx context.Context) error {
	a.log("%s: removing environment (%s)", a.environment, a.namespace())
	for _, release := range []string{"load", "apps", "migrations", "observability", "infra"} {
		a.log("%s: uninstalling release and waiting for cleanup", release)
		if err := a.uninstall(ctx, release, a.timeout); err != nil {
			return err
		}
	}
	if err := a.kubectl(ctx, "delete", "namespace", a.namespace(), "--ignore-not-found=true", "--wait=true"); err != nil {
		return err
	}
	a.log("%s: environment removal complete", a.environment)
	return nil
}

func (a *app) validate(ctx context.Context) error {
	a.log("Chart validation: preparing dependencies and configuration")
	if err := a.prepareCharts(ctx, "platform"); err != nil {
		return err
	}
	for _, env := range []string{"dev", "test", "qa"} {
		a.environment = env
		for _, group := range []string{"infra", "apps", "observability", "migrations", "load"} {
			a.log("%s/%s: linting and rendering chart", env, group)
			values, err := a.valuesArgs(group)
			if err != nil {
				return err
			}
			if err := a.runner.run(ctx, a.helm, append([]string{"lint", filepath.Join(a.stage, group), "--strict"}, values...)...); err != nil {
				return err
			}
			if _, err := a.render(ctx, group); err != nil {
				return err
			}
		}
	}
	a.log("Platform: linting and rendering chart")
	chart, values := filepath.Join(a.stage, "platform"), filepath.Join(a.helmDir(), "values", "common", "platform.yaml")
	if err := a.runner.run(ctx, a.helm, "lint", chart, "--strict", "-f", values); err != nil {
		return err
	}
	if _, err := a.runner.output(ctx, a.helm, "template", "platform", chart, "-n", "gochat-system", "-f", values); err != nil {
		return err
	}
	a.log("Chart validation complete (dev, test, qa and platform)")
	return nil
}
