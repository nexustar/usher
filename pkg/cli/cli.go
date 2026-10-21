// Package cli holds the usher subcommands other than serve: the hook and MCP
// stdio workers spawned sessions call back into, and the setup and password
// commands. usher.Main dispatches to them.
package cli

import "os"

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
