package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "Standalone registry observation is unsupported on Windows")
	os.Exit(1)
}
