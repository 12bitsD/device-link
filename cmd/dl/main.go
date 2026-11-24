package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/12bitsD/device-link/internal/client"
	"github.com/12bitsD/device-link/internal/hub"
	"github.com/12bitsD/device-link/internal/node"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		code := 1
		var codeErr exitCodeError
		if errors.As(err, &codeErr) {
			code = codeErr.code
		}
		if err.Error() != "" {
			fmt.Fprintln(os.Stderr, "dl:", err)
		}
		os.Exit(code)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dl <hub|invite|join|ls|ttl|rm|exec|job|cp|transfer> [flags]")
	}
	handler, ok := subcommands()[args[0]]
	if !ok {
		return fmt.Errorf("unknown command %q", args[0])
	}
	return handler(ctx, args[1:])
}

func subcommands() map[string]func(context.Context, []string) error {
	return map[string]func(context.Context, []string) error{
		"hub":      runHub,
		"invite":   runInvite,
		"join":     runJoin,
		"ls":       runList,
		"ttl":      runTTL,
		"rm":       runRemove,
		"exec":     runExec,
		"job":      runJob,
		"cp":       runCP,
		"transfer": runTransfer,
	}
}

func runHub(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("hub", flag.ContinueOnError)
	addr := flags.String("addr", ":8443", "HTTPS listen address")
	host := flags.String("host", "", "advertised host/IP added to the self-signed TLS certificate")
	stateDir := flags.String("state-dir", defaultStateDir("hub"), "hub state directory")
	if err := parseNoArgs(flags, args); err != nil {
		return err
	}
	return hub.Run(ctx, hub.Config{Addr: *addr, Host: *host, StateDir: *stateDir})
}

type controllerConfig struct {
	flags    *flag.FlagSet
	hubURL   string
	token    string
	caPath   string
	stateDir string
}

func addControllerFlags(flags *flag.FlagSet) *controllerConfig {
	config := &controllerConfig{flags: flags}
	flags.StringVar(&config.hubURL, "hub", "", "hub HTTPS URL")
	flags.StringVar(&config.token, "token", "", "controller token (defaults to DL_TOKEN or <state-dir>/controller_token)")
	flags.StringVar(&config.caPath, "ca", "", "hub CA certificate")
	flags.StringVar(&config.stateDir, "state-dir", defaultStateDir("hub"), "hub state directory for token/CA defaults")
	return config
}

func (c *controllerConfig) connect() (*client.Client, error) {
	if err := c.resolve(); err != nil {
		return nil, err
	}
	return client.New(c.hubURL, c.token, c.caPath)
}

func (c *controllerConfig) resolve() error {
	explicit := visitedFlags(c.flags)
	if !explicit["ca"] {
		defaultCA := filepath.Join(c.stateDir, "cert.pem")
		if _, err := os.Stat(defaultCA); err == nil {
			c.caPath = defaultCA
		}
	}
	if explicit["token"] {
		return nil
	}
	if envToken := os.Getenv("DL_TOKEN"); envToken != "" {
		c.token = envToken
		return nil
	}
	data, err := os.ReadFile(filepath.Join(c.stateDir, "controller_token"))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("controller token required (use --token, DL_TOKEN, or --state-dir): %w", err)
	}
	if err != nil {
		return fmt.Errorf("read controller token: %w", err)
	}
	c.token = strings.TrimSpace(string(data))
	return nil
}

func visitedFlags(flags *flag.FlagSet) map[string]bool {
	set := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) {
		set[f.Name] = true
	})
	return set
}

func parseNoArgs(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	return nil
}

func runInvite(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("invite", flag.ContinueOnError)
	config := addControllerFlags(flags)
	name := flags.String("name", "", "requested device name")
	ttlText := flags.String("ttl", "", "initial device TTL")
	if err := parseNoArgs(flags, args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("--name is required")
	}
	ttl, err := optionalDuration(*ttlText)
	if err != nil {
		return err
	}
	controller, err := config.connect()
	if err != nil {
		return err
	}
	code, err := controller.Invite(ctx, *name, ttl)
	if err != nil {
		return err
	}
	hubURL, _ := client.NormalizeHubURL(config.hubURL)
	if config.caPath != "" {
		fmt.Printf("dl join --hub %s --code %s --ca %s\n", quoteArg(hubURL), quoteArg(code), quoteArg(config.caPath))
		return nil
	}
	fmt.Printf("dl join --hub %s --code %s\n", quoteArg(hubURL), quoteArg(code))
	return nil
}

func quoteArg(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:=+-", r)
	}) == -1 {
		return value
	}
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return `'` + strings.ReplaceAll(value, `'`, `'"'"'`) + `'`
}

func runJoin(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("join", flag.ContinueOnError)
	hubURL := flags.String("hub", "", "hub HTTPS URL")
	code := flags.String("code", "", "one-time invite code")
	caPath := flags.String("ca", "", "hub CA certificate")
	stateDir := flags.String("state-dir", defaultStateDir("node"), "node state directory")
	if err := parseNoArgs(flags, args); err != nil {
		return err
	}
	config := node.Config{HubURL: *hubURL, Code: *code, CAPath: *caPath, StateDir: *stateDir}
	return node.Run(ctx, config)
}

func runList(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("ls", flag.ContinueOnError)
	config := addControllerFlags(flags)
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := parseNoArgs(flags, args); err != nil {
		return err
	}
	controller, err := config.connect()
	if err != nil {
		return err
	}
	devices, err := controller.List(ctx)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(devices)
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tSTATUS\tOS/ARCH\tSHELL\tEXPIRES\tLAST SEEN")
	for _, device := range devices {
		expires := "never"
		if device.ExpiresAt != nil {
			expires = *device.ExpiresAt
		}
		fmt.Fprintf(writer, "%s\t%s\t%s/%s\t%s\t%s\t%s\n", device.Name, device.Status, device.OS, device.Arch, device.Shell, expires, device.LastSeen)
	}
	return writer.Flush()
}

func runTTL(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("ttl", flag.ContinueOnError)
	config := addControllerFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return fmt.Errorf("usage: dl ttl [flags] <name> <duration|never>")
	}
	ttl, err := optionalDuration(flags.Arg(1))
	if err != nil {
		return err
	}
	controller, err := config.connect()
	if err != nil {
		return err
	}
	return controller.SetTTL(ctx, flags.Arg(0), ttl)
}

func runRemove(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("rm", flag.ContinueOnError)
	config := addControllerFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: dl rm [flags] <name>")
	}
	controller, err := config.connect()
	if err != nil {
		return err
	}
	return controller.Delete(ctx, flags.Arg(0))
}

func optionalDuration(value string) (*time.Duration, error) {
	if value == "" || value == "never" {
		return nil, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return nil, fmt.Errorf("invalid duration %q: %w", value, err)
	}
	if duration < 0 {
		return nil, fmt.Errorf("duration cannot be negative")
	}
	return &duration, nil
}

func defaultStateDir(role string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".device-link", role)
	}
	return filepath.Join(home, ".device-link", role)
}
