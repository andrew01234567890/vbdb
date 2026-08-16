// Command vbdb-operator is the future Kubernetes operator.
//
// The dependency-free binary exists so packaging and version checks can be
// wired now. Reconciliation is deliberately deferred to the operator
// milestone.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

const version = "0.1.0-m1"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("vbdb-operator", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	showVersion := fs.Bool("version", false, "print the vbdb-operator version")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if !*showVersion {
		return fmt.Errorf("reconciliation is not implemented in milestone 1; use --version")
	}
	fmt.Println(version)
	return nil
}
