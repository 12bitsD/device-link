package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/12bitsD/device-link/internal/client"
	"github.com/12bitsD/device-link/internal/transfer"
)

type patternList []string

func (p *patternList) String() string { return strings.Join(*p, ",") }

func (p *patternList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("--exclude pattern cannot be empty")
	}
	*p = append(*p, value)
	return nil
}

type cpSpec struct {
	from       string
	sources    []string
	to         string
	targetDir  string
	excludes   patternList
	wait       time.Duration
	detachMode bool
}

func runCP(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("cp", flag.ContinueOnError)
	config := addControllerFlags(flags)
	var spec cpSpec
	flags.Var(&spec.excludes, "exclude", "exclude glob pattern (repeatable)")
	flags.DurationVar(&spec.wait, "wait", 30*time.Second, "synchronous wait window")
	flags.BoolVar(&spec.detachMode, "detach", false, "return transfer_id immediately (remote-to-remote only)")
	flagArgs, posArgs := splitFlagsAndPositional(flags, args, -1)
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}
	if spec.wait < 0 {
		return fmt.Errorf("--wait cannot be negative")
	}
	if err := resolveCPEndpoints(posArgs, &spec); err != nil {
		return err
	}
	controller, err := config.connect()
	if err != nil {
		return err
	}
	status, err := executeCP(ctx, controller, spec)
	if err != nil {
		return err
	}
	return renderCPResult(spec, status)
}

func resolveCPEndpoints(posArgs []string, spec *cpSpec) error {
	if len(posArgs) < 2 {
		return fmt.Errorf("usage: dl cp [flags] [from:]<src>... [to:]<target_dir>")
	}
	target, err := client.ParseEndpoint(posArgs[len(posArgs)-1])
	if err != nil {
		return err
	}
	from, sources, err := parseSourceEndpoints(posArgs[:len(posArgs)-1])
	if err != nil {
		return err
	}
	if from == "local" && target.Device == "local" {
		return fmt.Errorf("at least one endpoint must be a remote device")
	}
	if spec.detachMode && (from == "local" || target.Device == "local") {
		return fmt.Errorf("--detach is only supported for remote-to-remote transfers")
	}
	spec.from = from
	spec.sources = sources
	spec.to = target.Device
	spec.targetDir = target.Path
	return nil
}

func parseSourceEndpoints(rawSources []string) (string, []string, error) {
	sources := make([]string, 0, len(rawSources))
	from := ""
	for _, raw := range rawSources {
		ep, err := client.ParseEndpoint(raw)
		if err != nil {
			return "", nil, err
		}
		if from == "" {
			from = ep.Device
		} else if ep.Device != from {
			return "", nil, fmt.Errorf("mixed source endpoints %q and %q", from, ep.Device)
		}
		sources = append(sources, ep.Path)
	}
	return from, sources, nil
}

func executeCP(ctx context.Context, controller *client.Client, spec cpSpec) (client.TransferStatus, error) {
	if spec.from == "local" {
		return executeLocalToRemote(ctx, controller, spec)
	}
	if spec.to == "local" {
		return executeRemoteToLocal(ctx, controller, spec)
	}
	waitMS := client.DurationMillis(spec.wait)
	if spec.detachMode {
		waitMS = 0
	}
	return controller.StartTransfer(ctx, client.TransferStartRequest{
		From:      spec.from,
		To:        spec.to,
		Sources:   spec.sources,
		Excludes:  spec.excludes,
		TargetDir: spec.targetDir,
		WaitMS:    waitMS,
	})
}

func executeLocalToRemote(ctx context.Context, controller *client.Client, spec cpSpec) (client.TransferStatus, error) {
	runCtx, cancel := context.WithTimeout(ctx, spec.wait)
	defer cancel()
	scan, err := transfer.ScanSources(runCtx, spec.sources, spec.excludes)
	if err != nil {
		return client.TransferStatus{}, err
	}
	started, err := controller.StartTransfer(runCtx, client.TransferStartRequest{
		From:       "local",
		To:         spec.to,
		TargetDir:  spec.targetDir,
		Candidates: scan.Candidates(),
		TotalBytes: scan.TotalBytes,
	})
	if err != nil {
		return client.TransferStatus{}, err
	}
	plan, err := transfer.FinalizePlan(runCtx, scan, started.ExistingHashes)
	if err != nil {
		controller.FailTransfer(ctx, started.TransferID, err.Error())
		return client.TransferStatus{}, err
	}
	status, uploadErr := controller.UploadTransfer(runCtx, started.TransferID, plan)
	if uploadErr != nil {
		controller.FailTransfer(ctx, started.TransferID, uploadErr.Error())
		return client.TransferStatus{}, uploadErr
	}
	return status, nil
}

func executeRemoteToLocal(ctx context.Context, controller *client.Client, spec cpSpec) (client.TransferStatus, error) {
	runCtx, cancel := context.WithTimeout(ctx, spec.wait)
	defer cancel()
	scanned, err := controller.ScanRemoteSources(runCtx, client.TransferScanRequest{
		Device:   spec.from,
		Sources:  spec.sources,
		Excludes: spec.excludes,
	})
	if err != nil {
		return client.TransferStatus{}, err
	}
	hashes, err := transfer.InspectTarget(runCtx, spec.targetDir, scanned.Candidates)
	if err != nil {
		return client.TransferStatus{}, err
	}
	started, err := controller.StartTransfer(runCtx, client.TransferStartRequest{
		From:           spec.from,
		To:             "local",
		Sources:        spec.sources,
		Excludes:       spec.excludes,
		ExistingHashes: hashes,
		TotalBytes:     scanned.TotalBytes,
	})
	if err != nil {
		return client.TransferStatus{}, err
	}
	status, dlErr := controller.DownloadTransfer(runCtx, started.TransferID, spec.targetDir)
	if dlErr != nil {
		controller.FailTransfer(ctx, started.TransferID, dlErr.Error())
		return client.TransferStatus{}, dlErr
	}
	return status, nil
}

func renderCPResult(spec cpSpec, status client.TransferStatus) error {
	if spec.detachMode || status.State == "running" {
		fmt.Printf("transfer_id=%s\n", status.TransferID)
		return nil
	}
	if status.State != "completed" {
		return fmt.Errorf("transfer %s %s", status.TransferID, status.State)
	}
	fmt.Printf("transfer_id=%s state=%s files=%d skipped=%d bytes=%d/%d\n",
		status.TransferID, status.State, status.Files, status.Skipped, status.Bytes, status.TotalBytes)
	return nil
}

func runTransfer(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dl transfer <status|cancel> [flags] <transfer_id>")
	}
	switch args[0] {
	case "status":
		return runTransferStatus(ctx, args[1:])
	case "cancel":
		return runTransferCancel(ctx, args[1:])
	default:
		return fmt.Errorf("unknown transfer command %q", args[0])
	}
}

func runTransferStatus(ctx context.Context, args []string) error {
	var jsonOut bool
	controller, transferID, err := parseIDCommand("transfer status", args, func(f *flag.FlagSet) {
		f.BoolVar(&jsonOut, "json", false, "print JSON")
	})
	if err != nil {
		return err
	}
	status, err := controller.TransferStatus(ctx, transferID)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(status)
	}
	fmt.Printf("transfer_id=%s from=%s to=%s state=%s files=%d skipped=%d bytes=%d/%d\n",
		status.TransferID, status.From, status.To, status.State,
		status.Files, status.Skipped, status.Bytes, status.TotalBytes)
	return nil
}

func runTransferCancel(ctx context.Context, args []string) error {
	controller, transferID, err := parseIDCommand("transfer cancel", args, nil)
	if err != nil {
		return err
	}
	return controller.TransferCancel(ctx, transferID)
}
