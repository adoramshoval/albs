package main

import (
	"os"

	"github.com/adoramshoval/albs/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
