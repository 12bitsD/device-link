package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/12bitsD/device-link/internal/client"
)

type exitCodeError struct {
	code int
	msg  string
}

func (e exitCodeError) Error() string { return e.msg }

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	if idx := strings.IndexByte(value, '='); idx <= 0 {
		return fmt.Errorf("invalid --env %q (expected KEY=VALUE)", value)
	}
	*s = append(*s, value)
	return nil
}

func runExec(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("exec", flag.ContinueOnError)
	config := addControllerFlags(flags)
	cwd := flags.String("cwd", "", "remote working directory")
	var env stringList
	flags.Var(&env, "env", "environment variable KEY=VALUE (repeatable)")
	wait := flags.Duration("wait", 10*time.Second, "synchronous wait window")
	timeout := flags.Duration("timeout", 0, "maximum remote command execution time")
	waitOnline := flags.Duration("wait-online", 0, "wait for offline device to reconnect")
	detach := flags.Bool("detach", false, "return job_id immediately without waiting")
	device, command, err := parseExecArgs(flags, args)
	if err != nil {
		return err
	}
	if *wait < 0 || *timeout < 0 || *waitOnline < 0 {
		return fmt.Errorf("durations cannot be negative")
	}
	waitMS := client.DurationMillis(*wait)
	if *detach {
		waitMS = 0
	}
	controller, err := config.connect()
	if err != nil {
		return err
	}
	result, err := controller.Exec(ctx, device, client.ExecRequest{
		Command:      command,
		Cwd:          *cwd,
		Env:          env,
		WaitMS:       waitMS,
		TimeoutMS:    client.DurationMillis(*timeout),
		WaitOnlineMS: client.DurationMillis(*waitOnline),
	})
	if err != nil {
		return err
	}
	return renderExecResult(result)
}

func renderExecResult(result client.ExecResult) error {
	if result.State == "running" {
		fmt.Printf("job_id=%s\n", result.JobID)
		return nil
	}
	_, _ = os.Stdout.Write(result.Stdout)
	_, _ = os.Stderr.Write(result.Stderr)
	switch result.State {
	case "completed":
		return nil
	case "timed_out":
		return exitCodeError{code: 124, msg: fmt.Sprintf("job %s timed out", result.JobID)}
	case "killed":
		return exitCodeError{code: 137, msg: fmt.Sprintf("job %s killed", result.JobID)}
	default:
		return exitCodeError{code: normalizeExitCode(result.ExitCode)}
	}
}

func normalizeExitCode(raw *int) int {
	if raw == nil || *raw <= 0 {
		return 1
	}
	if runtime.GOOS != "windows" && *raw > 255 {
		return 1
	}
	return *raw
}

func parseExecArgs(flags *flag.FlagSet, args []string) (string, []string, error) {
	flagArgs, posArgs := splitFlagsAndPositional(flags, args, 1)
	if err := flags.Parse(flagArgs); err != nil {
		return "", nil, err
	}
	if len(posArgs) < 2 || posArgs[0] == "" {
		return "", nil, fmt.Errorf("usage: dl exec [flags] <device> [--] <command> [args...]")
	}
	return posArgs[0], posArgs[1:], nil
}

func splitFlagsAndPositional(flags *flag.FlagSet, args []string, maxPosForFlags int) ([]string, []string) {
	var flagArgs []string
	var posArgs []string
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			return flagArgs, append(posArgs, args[i+1:]...)
		}
		if (maxPosForFlags < 0 || len(posArgs) <= maxPosForFlags) && strings.HasPrefix(tok, "-") && tok != "-" {
			flagArgs = append(flagArgs, tok)
			if flagTakesValue(flags, tok) && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		posArgs = append(posArgs, tok)
	}
	return flagArgs, posArgs
}

func flagTakesValue(flags *flag.FlagSet, token string) bool {
	name := strings.TrimLeft(token, "-")
	if strings.Contains(name, "=") {
		return false
	}
	f := flags.Lookup(name)
	if f == nil {
		return false
	}
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !bf.IsBoolFlag()
}

func runJob(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dl job <status|logs|kill> [flags] <job_id>")
	}
	switch args[0] {
	case "status":
		return runJobStatus(ctx, args[1:])
	case "logs":
		return runJobLogs(ctx, args[1:])
	case "kill":
		return runJobKill(ctx, args[1:])
	default:
		return fmt.Errorf("unknown job command %q", args[0])
	}
}

func parseIDCommand(subcmd string, args []string, extra func(*flag.FlagSet)) (*client.Client, string, error) {
	flags := flag.NewFlagSet(subcmd, flag.ContinueOnError)
	config := addControllerFlags(flags)
	if extra != nil {
		extra(flags)
	}
	flagArgs, posArgs := splitFlagsAndPositional(flags, args, -1)
	if err := flags.Parse(flagArgs); err != nil {
		return nil, "", err
	}
	if len(posArgs) != 1 || posArgs[0] == "" {
		return nil, "", fmt.Errorf("usage: dl %s [flags] <id>", subcmd)
	}
	controller, err := config.connect()
	if err != nil {
		return nil, "", err
	}
	return controller, posArgs[0], nil
}

func runJobStatus(ctx context.Context, args []string) error {
	var jsonOut bool
	controller, jobID, err := parseIDCommand("job status", args, func(f *flag.FlagSet) {
		f.BoolVar(&jsonOut, "json", false, "print JSON")
	})
	if err != nil {
		return err
	}
	status, err := controller.JobStatus(ctx, jobID)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(status)
	}
	codeText := "none"
	if status.ExitCode != nil {
		codeText = fmt.Sprintf("%d", *status.ExitCode)
	}
	fmt.Printf("job_id=%s device=%s state=%s exit_code=%s bytes=%d\n",
		status.JobID, status.Device, status.State, codeText, status.Bytes)
	return nil
}

func runJobLogs(ctx context.Context, args []string) error {
	var offset int64
	controller, jobID, err := parseIDCommand("job logs", args, func(f *flag.FlagSet) {
		f.Int64Var(&offset, "offset", 0, "byte offset to resume reading from")
	})
	if err != nil {
		return err
	}
	if offset < 0 {
		return fmt.Errorf("--offset cannot be negative")
	}
	return controller.JobLogs(ctx, jobID, offset, os.Stdout)
}

func runJobKill(ctx context.Context, args []string) error {
	controller, jobID, err := parseIDCommand("job kill", args, nil)
	if err != nil {
		return err
	}
	return controller.JobKill(ctx, jobID)
}
