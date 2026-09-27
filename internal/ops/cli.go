package ops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var commands = []struct{ name, description string }{
	{"dev-up", "Build images and deploy dev"},
	{"test-up", "Build images and deploy test"},
	{"qa-up", "Build images and deploy qa"},
	{"dev-down", "Remove dev and wait for cleanup"},
	{"test-down", "Remove test and wait for cleanup"},
	{"qa-down", "Remove qa and wait for cleanup"},
	{"dev-load", "Run the C10K load test (4 workers)"},
	{"qa-load", "Run the HPA scaling and reconnection test"},
	{"kind-up", "Prepare the cluster and shared platform"},
	{"kind-delete", "Delete the local cluster and its data"},
	{"helm-validate", "Lint and render charts for all environments without a cluster"},
}

type config struct {
	command, environment            string
	root, cluster, kindConfig, helm string
	timeout, loadTimeout            time.Duration
	followLogs                      bool
	maxLogRequests                  int
	loadValues                      []string
}

func (c config) namespace() string   { return "go-chat-" + c.environment }
func (c config) kubeContext() string { return "kind-" + c.cluster }
func (c config) helmDir() string     { return filepath.Join(c.root, "deploy", "helm") }

type app struct {
	config
	runner   *runner
	stage    string
	terminal bool
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	c, err := parseConfig(args, os.Getenv, stdout)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	c.root, err = os.Getwd()
	if err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join(c.helmDir(), "charts", "platform", "Chart.yaml")); err != nil {
		return errors.New("run ops from the repository root")
	}
	output := &lockedWriter{writer: stdout}
	a := &app{config: c, runner: &runner{dir: c.root, out: output, errOut: &lockedWriter{writer: stderr}}}
	if f, ok := stdout.(*os.File); ok {
		info, statErr := f.Stat()
		a.terminal = statErr == nil && info.Mode()&os.ModeCharDevice != 0 && os.Getenv("TERM") != "dumb"
	}
	defer a.cleanup()
	if err = a.checkTools(ctx); err != nil {
		return err
	}
	switch {
	case c.command == "kind-up":
		return a.kindUp(ctx)
	case c.command == "kind-delete":
		a.log("Deleting cluster and its data: %s; waiting for completion", c.cluster)
		if err := a.runner.run(ctx, "kind", "delete", "cluster", "--name", c.cluster); err != nil {
			return err
		}
		a.log("Cluster deletion complete: %s", c.cluster)
		return nil
	case c.command == "helm-validate":
		return a.validate(ctx)
	case strings.HasSuffix(c.command, "-up"):
		if err := a.kindUp(ctx); err != nil {
			return err
		}
		return a.up(ctx)
	case strings.HasSuffix(c.command, "-down"):
		return a.down(ctx)
	default:
		return a.load(ctx)
	}
}

func parseConfig(args []string, getenv func(string) string, out io.Writer) (config, error) {
	var c config
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, _ = fmt.Fprintln(out, "Usage: go run ./cmd/ops <command> [options]\n\nCommands:")
		for _, cmd := range commands {
			_, _ = fmt.Fprintf(out, "  %-16s %s\n", cmd.name, cmd.description)
		}
		_, _ = fmt.Fprintln(out, "\nRun a command with --help for options and environment variables.")
		return c, flag.ErrHelp
	}
	c.command = args[0]
	if !slices.ContainsFunc(commands, func(cmd struct{ name, description string }) bool { return cmd.name == c.command }) {
		return c, fmt.Errorf("unknown command %q; use --help", c.command)
	}
	c.environment = strings.SplitN(c.command, "-", 2)[0]
	isLoad := strings.HasSuffix(c.command, "-load")
	isUp := strings.HasSuffix(c.command, "-up")
	isValidate := c.command == "helm-validate"
	env := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	c.helm = env("HELM", "helm")
	c.cluster = env("KIND_CLUSTER", "go-chat")
	c.kindConfig = env("KIND_CONFIG", "deploy/k8s/clusters/kind-local.yaml")
	timeout := env("KUBECTL_TIMEOUT", "300s")
	loadDefault, logsDefault := "30m", "4"
	if c.environment == "qa" {
		loadDefault, logsDefault = "15m", "1"
	}
	loadTimeout := env("K6_LOAD_TIMEOUT", loadDefault)
	follow := env("K6_FOLLOW_LOGS", "false")
	maxLogs := env("K6_MAX_LOG_REQUESTS", logsDefault)
	fs := flag.NewFlagSet(c.command, flag.ContinueOnError)
	fs.SetOutput(out)
	if !isValidate {
		fs.StringVar(&c.cluster, "cluster", c.cluster, "kind cluster name (KIND_CLUSTER)")
	}
	if isUp {
		fs.StringVar(&c.kindConfig, "config", c.kindConfig, "kind configuration file (KIND_CONFIG)")
	}
	if isLoad {
		fs.StringVar(&loadTimeout, "timeout", loadTimeout, "test completion timeout (K6_LOAD_TIMEOUT)")
		fs.BoolVar(&c.followLogs, "follow-logs", false, "stream k6 logs (K6_FOLLOW_LOGS; default "+follow+")")
		fs.StringVar(&maxLogs, "max-log-requests", maxLogs, "concurrent log requests (K6_MAX_LOG_REQUESTS)")
	} else if !isValidate && c.command != "kind-delete" {
		fs.StringVar(&timeout, "timeout", timeout, "resource readiness timeout (KUBECTL_TIMEOUT)")
	}
	fs.Usage = func() {
		_, _ = fmt.Fprintf(out, "Usage: go run ./cmd/ops %s [options]\n\n", c.command)
		fs.PrintDefaults()
		if c.command != "kind-delete" {
			_, _ = fmt.Fprintln(out, "\nEnvironment: HELM (default helm)")
		}
		if isLoad {
			_, _ = fmt.Fprintln(out, "K6_WORKER_VUS, K6_RAMP_DURATION, K6_PLATEAU_DURATION, K6_RAMP_DOWN_DURATION\nUnset load values use the environment's Helm values.")
		}
		_, _ = fmt.Fprintf(out, "\nExample: go run ./cmd/ops %s\n", c.command)
	}
	if err := fs.Parse(args[1:]); err != nil {
		return c, err
	}
	if fs.NArg() != 0 {
		return c, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if !isValidate && strings.TrimSpace(c.cluster) == "" {
		return c, errors.New("cluster must not be empty")
	}
	if isUp && c.kindConfig == "" {
		return c, errors.New("config must not be empty")
	}
	c.timeout = 300 * time.Second
	var err error
	if !isValidate && c.command != "kind-delete" {
		c.timeout, err = positiveDuration(timeout)
		if err != nil {
			return c, fmt.Errorf("KUBECTL_TIMEOUT/--timeout: %w", err)
		}
	}
	if isLoad {
		c.loadTimeout, err = positiveDuration(loadTimeout)
		if err != nil {
			return c, fmt.Errorf("K6_LOAD_TIMEOUT/--timeout: %w", err)
		}
		explicitFollow := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "follow-logs" {
				explicitFollow = true
			}
		})
		if !explicitFollow {
			c.followLogs, err = strconv.ParseBool(follow)
			if err != nil {
				return c, fmt.Errorf("invalid K6_FOLLOW_LOGS: %w", err)
			}
		}
		c.maxLogRequests, err = strconv.Atoi(maxLogs)
		if err != nil || c.maxLogRequests <= 0 {
			return c, errors.New("K6_MAX_LOG_REQUESTS/--max-log-requests must be a positive integer")
		}
		for _, key := range []string{"K6_WORKER_VUS", "K6_RAMP_DURATION", "K6_PLATEAU_DURATION", "K6_RAMP_DOWN_DURATION"} {
			value := getenv(key)
			if value == "" {
				continue
			}
			pattern := `^[1-9][0-9]*[smh]$`
			if key == "K6_WORKER_VUS" {
				pattern = `^[1-9][0-9]*$`
			}
			if !regexp.MustCompile(pattern).MatchString(value) {
				return c, fmt.Errorf("invalid %s: %q", key, value)
			}
			c.loadValues = append(c.loadValues, "--set-string", "env."+key+"="+value)
		}
	}
	return c, nil
}

func positiveDuration(value string) (time.Duration, error) {
	if _, err := strconv.ParseUint(value, 10, 64); err == nil {
		value += "s"
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("expected a positive duration, got %q", value)
	}
	return d, nil
}

func (a *app) checkTools(ctx context.Context) error {
	names := []string{a.helm}
	switch {
	case a.command == "kind-delete":
		names = []string{"kind"}
	case strings.HasSuffix(a.command, "-up"):
		names = append(names, "docker", "kind", "kubectl")
	case a.command != "helm-validate":
		names = append(names, "kubectl")
	}
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("required tool not found: %s; install it and try again: %w", name, err)
		}
	}
	if a.command == "kind-delete" {
		return nil
	}
	version, err := a.runner.output(ctx, a.helm, "version", "--template", "{{.Version}}")
	if err != nil {
		return err
	}
	match := regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.[0-9]+(\+[0-9A-Za-z.-]+)?$`).FindStringSubmatch(strings.TrimSpace(string(version)))
	if len(match) > 0 {
		major, _ := strconv.Atoi(match[1])
		minor, _ := strconv.Atoi(match[2])
		if major > 4 || major == 4 && minor >= 3 {
			return nil
		}
	}
	return fmt.Errorf("helm 4.3.0 or newer is required (found %s)", strings.TrimSpace(string(version)))
}

func (a *app) log(format string, args ...any) {
	_, _ = fmt.Fprintf(a.runner.out, "[ops] "+format+"\n", args...)
}
func (a *app) kubectl(ctx context.Context, args ...string) error {
	return a.runner.run(ctx, "kubectl", append([]string{"--context", a.kubeContext()}, args...)...)
}
func (a *app) kubeOutput(ctx context.Context, args ...string) ([]byte, error) {
	return a.runner.output(ctx, "kubectl", append([]string{"--context", a.kubeContext()}, args...)...)
}
func (a *app) cleanup() {
	if a.stage != "" {
		if err := os.RemoveAll(a.stage); err != nil {
			_, _ = fmt.Fprintf(a.runner.errOut, "remove temporary charts: %v\n", err)
		}
		a.stage = ""
	}
}
