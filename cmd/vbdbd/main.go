// Command vbdbd is the future VBDB data-plane process.
//
// Milestone 1 only defines and validates the process role contract. Serving
// traffic is intentionally not implemented yet; accepting a role and then
// claiming to serve it would make this scaffold unsafe to deploy.
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
	fs := flag.NewFlagSet("vbdbd", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	role := fs.String("role", "", "process role: gateway, metadata, or storage")
	showVersion := fs.Bool("version", false, "print the vbdbd version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if *showVersion {
		if *role != "" {
			return errors.New("--version cannot be combined with --role")
		}
		fmt.Println(version)
		return nil
	}
	if *role != "gateway" && *role != "metadata" && *role != "storage" {
		return errors.New("--role is required and must be one of gateway, metadata, storage")
	}
	return fmt.Errorf("vbdbd role %q is not implemented in milestone 1", *role)
}
