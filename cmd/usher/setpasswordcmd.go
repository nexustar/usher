package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/nexustar/usher/pkg/auth"
)

// runSetPassword is the worker for `usher set-password`. It writes an
// argon2id hash of the supplied password to <data-dir>/auth.json (0600).
// Once auth.json exists, `usher serve` requires login on every request.
//
// Two input modes:
//   - interactive (default): prompt twice on the terminal with echo off.
//   - --password-stdin: read one line from stdin, no confirmation. For
//     scripting / Ansible / etc.
//
// Plaintext is never accepted as a flag or env var — both leak to shell
// history, /proc/<pid>/environ, and CI logs.
func runSetPassword(args []string) error {
	fs := flag.NewFlagSet("set-password", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "usher data directory (XDG_DATA_HOME/usher)")
	stdinMode := fs.Bool("password-stdin", false, "read password from stdin (single line, no confirmation)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" {
		return fmt.Errorf("could not resolve data dir; pass --data-dir")
	}

	store, err := auth.Load(*dataDir)
	if err != nil {
		return err
	}

	var pw string
	if *stdinMode {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		pw = strings.TrimRight(string(raw), "\r\n")
	} else {
		pw, err = promptPasswordTwice()
		if err != nil {
			return err
		}
	}

	if err := store.SetPassword(pw); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "password saved to %s\n", filepath.Join(*dataDir, "auth.json"))
	fmt.Fprintln(os.Stderr, "all existing browser sessions have been kicked.")
	return nil
}

func promptPasswordTwice() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("stdin is not a terminal; pipe with --password-stdin instead")
	}
	fmt.Fprint(os.Stderr, "New password: ")
	pw1, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if len(pw1) == 0 {
		return "", fmt.Errorf("password must not be empty")
	}
	fmt.Fprint(os.Stderr, "Confirm password: ")
	pw2, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read confirmation: %w", err)
	}
	if string(pw1) != string(pw2) {
		return "", fmt.Errorf("passwords do not match")
	}
	return string(pw1), nil
}
