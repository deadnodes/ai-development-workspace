package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "Native publication is unsupported on Windows")
	os.Exit(1)
}
