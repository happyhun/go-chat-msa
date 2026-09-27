package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
)

type jobStatus struct {
	Spec struct {
		Parallelism *int `json:"parallelism"`
		Completions *int `json:"completions"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

func (j jobStatus) parallelism() int {
	if j.Spec.Parallelism != nil {
		return *j.Spec.Parallelism
	}
	return 1
}
func (j jobStatus) expected() int {
	if j.Spec.Completions != nil {
		return *j.Spec.Completions
	}
	return j.parallelism()
}
func (j jobStatus) result() (string, string) {
	for _, condition := range j.Status.Conditions {
		if condition.Status == "True" && condition.Type == "Failed" {
			return "Failed", strings.TrimSpace(condition.Reason + " " + condition.Message)
		}
	}
	for _, condition := range j.Status.Conditions {
		if condition.Status == "True" && condition.Type == "Complete" {
			return "Complete", ""
		}
	}
	return "", ""
}

type containerState struct {
	Waiting *struct {
		Reason string `json:"reason"`
	} `json:"waiting"`
	Running *struct {
		StartedAt string `json:"startedAt"`
	} `json:"running"`
	Terminated *struct {
		Reason   string `json:"reason"`
		ExitCode int    `json:"exitCode"`
	} `json:"terminated"`
}

type podStatus struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			Name  string         `json:"name"`
			State containerState `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func (p podStatus) k6State() containerState {
	for _, container := range p.Status.ContainerStatuses {
		if container.Name == "k6" {
			return container.State
		}
	}
	return containerState{}
}

type podList struct {
	Items []podStatus `json:"items"`
}

func workerProgress(pods podList, job jobStatus) (phase, detail string, ready bool) {
	var running, succeeded, failed, pending int
	var reasons []string
	ready = len(pods.Items) >= job.parallelism() && len(pods.Items) > 0
	for _, pod := range pods.Items {
		state := pod.k6State()
		name := pod.Metadata.Name
		switch {
		case state.Terminated != nil:
			if state.Terminated.ExitCode == 0 {
				succeeded++
			} else {
				failed++
				reasons = append(reasons, fmt.Sprintf("%s:%s(exit=%d)", name, state.Terminated.Reason, state.Terminated.ExitCode))
			}
		case pod.Status.Phase == "Failed":
			failed++
			reasons = append(reasons, name+":Failed")
			ready = false
		case state.Running != nil:
			running++
		default:
			pending++
			ready = false
			reason := pod.Status.Phase
			if state.Waiting != nil && state.Waiting.Reason != "" {
				reason = state.Waiting.Reason
			}
			if reason == "" {
				reason = "Pending"
			}
			reasons = append(reasons, name+":"+reason)
		}
	}
	phase = "Running"
	if pending > 0 || running+succeeded+failed < job.expected() {
		phase = "Preparing"
	} else if succeeded+failed > 0 {
		phase = "Finishing"
	}
	if result, _ := job.result(); result != "" {
		phase = result
	}
	detail = fmt.Sprintf("workers: %d running, %d succeeded, %d failed / %d", running, succeeded, failed, job.expected())
	if len(reasons) > 0 {
		detail += " | " + strings.Join(reasons, " ")
	}
	return phase, detail, ready
}

type progress struct {
	out        io.Writer
	terminal   bool
	started    time.Time
	printed    time.Time
	key, phase string
	active     bool
}

func (p *progress) finish() {
	if p.active {
		_, _ = fmt.Fprintln(p.out)
		p.active = false
	}
}

func (p *progress) show(phase, detail string) {
	elapsed := int(time.Since(p.started).Seconds())
	line := fmt.Sprintf("%s | elapsed %02d:%02d | %s", phase, elapsed/60, elapsed%60, detail)
	key := phase + "|" + detail
	if p.terminal {
		if phase != p.phase {
			p.finish()
		}
		_, _ = fmt.Fprintf(p.out, "\r\033[2K%s", line)
		p.active = true
	} else if key != p.key || time.Since(p.printed) >= 30*time.Second {
		_, _ = fmt.Fprintf(p.out, "[%s] %s\n", time.Now().Format("15:04:05"), line)
		p.printed = time.Now()
	}
	p.key, p.phase = key, phase
}

func (a *app) load(ctx context.Context) error {
	a.log("%s: preparing load test charts and configuration", a.environment)
	if err := a.prepareCharts(ctx); err != nil {
		return err
	}
	jobName := "k6-c10k"
	if a.environment == "qa" {
		jobName = "k6-hpa"
	}
	a.log("Removing previous load test resources and waiting for cleanup")
	if err := a.uninstall(ctx, "load", 120*time.Second); err != nil {
		return err
	}
	if err := a.resetHPA(ctx); err != nil {
		return err
	}
	a.log("%s: creating load test Job", jobName)
	values, err := a.valuesArgs("load")
	if err != nil {
		return err
	}
	args := []string{"install", "load", a.stage + "/load", "--kube-context", a.kubeContext(), "-n", a.namespace(), "--timeout", a.loadTimeout.String()}
	args = append(args, values...)
	args = append(args, a.loadValues...)
	if err := a.runner.run(ctx, a.helm, args...); err != nil {
		return err
	}
	a.log("%s: monitoring test progress (timeout: %s)", jobName, a.loadTimeout)
	monitorErr := a.monitorJob(ctx, jobName)
	if ctx.Err() != nil {
		return monitorErr
	}
	if monitorErr != nil {
		_ = a.kubectl(ctx, "--request-timeout=5s", "-n", a.namespace(), "describe", "job/"+jobName)
		_ = a.kubectl(ctx, "--request-timeout=5s", "-n", a.namespace(), "get", "pods", "-l", "job-name="+jobName, "-o", "wide")
	} else {
		a.log("%s: load test complete; %s succeeded", a.environment, jobName)
	}
	if err := a.printResults(ctx, jobName); err != nil {
		a.log("Unable to retrieve final load test metrics: %v", err)
	}
	return monitorErr
}

func (a *app) resetHPA(ctx context.Context) (err error) {
	if a.environment != "qa" {
		return nil
	}
	manifest, err := a.render(ctx, "apps", "--show-only", "charts/websocket-service/templates/hpa.yaml")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(manifest)) == "" {
		return errors.New("rendered HPA is empty")
	}
	restore := true
	defer func() {
		if !restore {
			return
		}
		// Recovery must still run after Ctrl+C cancels the deployment context.
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if restoreErr := a.apply(recovery, manifest, "-n", a.namespace()); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore WebSocket HPA: %w", restoreErr))
		}
	}()
	a.log("HPA test: resetting WebSocket to one Pod and waiting for readiness")
	if err = a.kubectl(ctx, "-n", a.namespace(), "delete", "hpa", "websocket-service", "--ignore-not-found=true", "--wait=true"); err != nil {
		return err
	}
	if err = a.kubectl(ctx, "-n", a.namespace(), "scale", "deployment/websocket-service", "--replicas=1"); err != nil {
		return err
	}
	if err = a.kubectl(ctx, "-n", a.namespace(), "rollout", "status", "deployment/websocket-service", "--timeout=120s"); err != nil {
		return err
	}
	if err = a.apply(ctx, manifest, "-n", a.namespace()); err != nil {
		return err
	}
	restore = false
	return a.kubectl(ctx, "-n", a.namespace(), "wait", "--for=condition=AbleToScale", "hpa/websocket-service", "--timeout=120s")
}

func (a *app) readStatus(ctx context.Context, target any, args ...string) error {
	request, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	prefix := []string{"--request-timeout=5s", "-n", a.namespace(), "get"}
	data, err := a.kubeOutput(request, append(append(prefix, args...), "-o", "json")...)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func (a *app) monitorJob(ctx context.Context, name string) error {
	monitor, cancel := context.WithTimeout(ctx, a.loadTimeout)
	defer cancel()
	display := &progress{out: a.runner.out, terminal: a.terminal && !a.followLogs, started: time.Now()}
	defer display.finish()
	var logs *process
	defer func() { logs.stop() }()
	for monitor.Err() == nil {
		var job jobStatus
		if err := a.readStatus(monitor, &job, "job/"+name); err != nil {
			display.show("Retrying", "Unable to fetch job/"+name+" status")
		} else {
			var pods podList
			ready := false
			state, reason := job.result()
			if err := a.readStatus(monitor, &pods, "pods", "-l", "job-name="+name); err != nil {
				if state == "" {
					display.show("Retrying", "Unable to fetch worker status")
				} else {
					display.show(state, "job/"+name+" finished (worker details unavailable)")
				}
			} else {
				phase, detail, workersReady := workerProgress(pods, job)
				ready = workersReady
				display.show(phase, detail)
			}
			if state == "Complete" {
				return nil
			}
			if state == "Failed" {
				return fmt.Errorf("job/%s failed: %s", name, reason)
			}
			if logs != nil {
				select {
				case <-logs.done:
					logs.stop()
					logs = nil
				default:
				}
			}
			if a.followLogs && ready && logs == nil && monitor.Err() == nil {
				deadline, _ := monitor.Deadline()
				remaining := max(time.Until(deadline).Round(time.Second), time.Second)
				var err error
				logs, err = a.runner.start(monitor, "kubectl", "--context", a.kubeContext(), "-n", a.namespace(), "logs", "-l", "job-name="+name,
					"--follow", "--all-containers=true", "--prefix=true", "--tail=-1", "--pod-running-timeout="+remaining.String(), fmt.Sprintf("--max-log-requests=%d", a.maxLogRequests))
				if err != nil {
					a.log("Unable to follow logs; will retry: %v", err)
				}
			}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-monitor.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	display.show("Timed out", "job/"+name+": "+a.loadTimeout.String())
	return fmt.Errorf("timed out waiting for job/%s after %s: %w", name, a.loadTimeout, monitor.Err())
}

func resultSummary(logs string) (string, bool) {
	var result strings.Builder
	found := false
	for line := range strings.SplitSeq(logs, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trimmed, "█ THRESHOLDS") || strings.HasPrefix(trimmed, "█ TOTAL RESULTS") {
			found = true
		}
		if found && (strings.HasPrefix(line, "running ") || strings.HasPrefix(line, "interrupted ")) {
			break
		}
		if found && (line == "" || unicode.IsSpace([]rune(line)[0])) {
			_, _ = fmt.Fprintln(&result, line)
		}
	}
	return result.String(), found
}

func (a *app) printResults(ctx context.Context, name string) error {
	data, err := a.kubeOutput(ctx, "--request-timeout=5s", "-n", a.namespace(), "get", "pods", "-l", "job-name="+name, "--sort-by=.metadata.name", "-o", "name")
	if err != nil {
		return err
	}
	pods := strings.Fields(string(data))
	if len(pods) == 0 {
		return errors.New("no load test pods found")
	}
	for _, pod := range pods {
		_, _ = fmt.Fprintf(a.runner.out, "\n--- %s ---\n", pod)
		logs, err := a.kubeOutput(ctx, "--request-timeout=15s", "-n", a.namespace(), "logs", pod, "-c", "k6", "--tail=-1")
		summary, found := resultSummary(string(logs))
		if err == nil && found {
			_, _ = fmt.Fprint(a.runner.out, summary)
		} else {
			_, _ = fmt.Fprintf(a.runner.errOut, "Final metrics unavailable; inspect with: kubectl --context %s -n %s logs %s -c k6\n", a.kubeContext(), a.namespace(), pod)
		}
	}
	return nil
}
