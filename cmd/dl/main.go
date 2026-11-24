package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: dl <hub|invite|join|ls|ttl|rm|exec|job|cp|transfer> [flags]")
	}
	return fmt.Errorf("command %q is not implemented yet", args[0])
}
