package main

import (
	"fmt"
	"os"

	"github.com/rojinebrahimi/sesh/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "sesh:", err)
		os.Exit(1)
	}
}
