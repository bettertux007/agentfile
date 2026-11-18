package main

import (
	"os"

	"github.com/bettertux007/agentfile/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:]))
}
