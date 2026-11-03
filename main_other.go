//go:build !unix

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "clauder supports macOS and Linux only")
	os.Exit(1)
}
