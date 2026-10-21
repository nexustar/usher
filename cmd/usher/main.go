// usher is the entrypoint binary; package usher at the module root does the
// work, so a custom binary with extensions is the same one line.
package main

import (
	"os"

	"github.com/nexustar/usher"
)

func main() { os.Exit(usher.Main(os.Args[1:])) }
