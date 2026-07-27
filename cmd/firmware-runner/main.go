package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jamesbraid/unifi-emu/internal/firmwarerunner"
)

type commandFunc func(context.Context, string, ...string) ([]byte, error)
type waitFunc func(context.Context, time.Duration) error

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, realCommand); err != nil {
		fmt.Fprintln(os.Stderr, "firmware-runner:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer, command commandFunc) error {
	flags := flag.NewFlagSet("firmware-runner", flag.ContinueOnError)
	flags.SetOutput(stdout)
	profilePath := flags.String("profile", "", "runner profile JSON")
	metadataPath := flags.String("rootfs-json", "", "extractor rootfs.json")
	tarPath := flags.String("rootfs-tar", "", "extractor rootfs.tar")
	workDir := flags.String("work-dir", "", "new disposable runtime directory")
	network := flags.String("network", "", "existing Docker network")
	informURL := flags.String("inform-url", "", "controller inform URL")
	dryRun := flags.Bool("dry-run", false, "prepare and print the launch plan without invoking Docker")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	for name, value := range map[string]string{
		"profile":     *profilePath,
		"rootfs-json": *metadataPath,
		"rootfs-tar":  *tarPath,
		"work-dir":    *workDir,
		"network":     *network,
	} {
		if value == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	profile, err := firmwarerunner.LoadProfile(*profilePath)
	if err != nil {
		return err
	}
	if profileNeedsInform(profile) && *informURL == "" {
		return errors.New("-inform-url is required by this profile")
	}
	bundle, err := firmwarerunner.LoadBundle(*metadataPath, *tarPath)
	if err != nil {
		return err
	}
	plan, err := firmwarerunner.Prepare(firmwarerunner.Options{
		Bundle: bundle, Profile: profile, WorkDir: *workDir,
		Network: *network, InformURL: *informURL,
	})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(plan); err != nil {
		return fmt.Errorf("print launch plan: %w", err)
	}
	if *dryRun {
		return nil
	}
	if command == nil {
		command = realCommand
	}
	return executePlan(ctx, plan, command)
}

func executePlan(
	ctx context.Context,
	plan firmwarerunner.Plan,
	command commandFunc,
) error {
	return executePlanWithWait(ctx, plan, command, waitForDuration)
}

func executePlanWithWait(
	ctx context.Context,
	plan firmwarerunner.Plan,
	command commandFunc,
	wait waitFunc,
) (resultErr error) {
	output, err := command(ctx, "docker", plan.DockerArgs()...)
	if err != nil {
		return commandError("start firmware container", output, err)
	}
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		output, err := command(
			cleanupCtx, "docker", "rm", "-f", plan.ContainerName,
		)
		if err != nil {
			cleanupErr := commandError("remove failed firmware container", output, err)
			resultErr = fmt.Errorf("%w; cleanup failed: %v", resultErr, cleanupErr)
		}
	}()
	if err := wait(
		ctx, time.Duration(plan.StartupGraceSeconds)*time.Second,
	); err != nil {
		return fmt.Errorf("wait for firmware startup grace period: %w", err)
	}
	if err := requireRunningContainer(ctx, plan.ContainerName, command); err != nil {
		return err
	}
	if plan.PostStart == nil {
		succeeded = true
		return nil
	}

	waitCtx, cancel := context.WithTimeout(
		ctx, time.Duration(plan.PostStart.TimeoutSeconds)*time.Second,
	)
	defer cancel()
	var lastOutput []byte
	var lastErr error
	for {
		lastOutput, lastErr = command(
			waitCtx, "docker", "exec", plan.ContainerName,
			"test", "-S", plan.PostStart.Socket,
		)
		if lastErr == nil {
			break
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return commandError("wait for firmware control socket", lastOutput, waitCtx.Err())
		case <-timer.C:
		}
	}
	args := append([]string{"exec", plan.ContainerName}, plan.PostStart.Command...)
	output, err = command(ctx, "docker", args...)
	if err != nil {
		return commandError("run firmware post-start command", output, err)
	}
	succeeded = true
	return nil
}

func requireRunningContainer(
	ctx context.Context,
	containerName string,
	command commandFunc,
) error {
	stateOutput, inspectErr := command(
		ctx, "docker", "inspect", "--format", "{{json .State}}", containerName,
	)
	var state struct {
		Running  bool   `json:"Running"`
		Status   string `json:"Status"`
		ExitCode int    `json:"ExitCode"`
		Error    string `json:"Error"`
	}
	decodeErr := json.Unmarshal(stateOutput, &state)
	if inspectErr == nil && decodeErr == nil && state.Running {
		return nil
	}
	logOutput, logErr := command(
		ctx, "docker", "logs", "--tail", "100", containerName,
	)
	details := []string{
		"state=" + strings.TrimSpace(string(stateOutput)),
		"logs=" + strings.TrimSpace(string(logOutput)),
	}
	if logErr != nil {
		details = append(details, "logs_error="+logErr.Error())
	}
	switch {
	case inspectErr != nil:
		return fmt.Errorf(
			"inspect firmware container: %w: %s",
			inspectErr, strings.Join(details, "; "),
		)
	case decodeErr != nil:
		return fmt.Errorf(
			"decode firmware container state: %w: %s",
			decodeErr, strings.Join(details, "; "),
		)
	default:
		return fmt.Errorf(
			"firmware container stopped during startup: status=%s exit_code=%d error=%q: %s",
			state.Status, state.ExitCode, state.Error, strings.Join(details, "; "),
		)
	}
}

func waitForDuration(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func profileNeedsInform(profile firmwarerunner.Profile) bool {
	for _, arg := range profile.Command {
		if strings.Contains(arg, "{inform_url}") {
			return true
		}
	}
	if profile.PostStart != nil {
		for _, arg := range profile.PostStart.Command {
			if strings.Contains(arg, "{inform_url}") {
				return true
			}
		}
	}
	return false
}

func commandError(action string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, detail)
}

func realCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
