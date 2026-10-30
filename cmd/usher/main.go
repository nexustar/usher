// usher is the entrypoint binary; package usher at the module root does the
// work. A program embedding usher builds on usher.New instead (see its
// Example).
package main

import (
	"os"

	"github.com/nexustar/usher"
)

func main() { os.Exit(usher.Main(os.Args[1:])) }
