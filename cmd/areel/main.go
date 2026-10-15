package main

import (
	"errors"
	"fmt"
	"github.com/self-deprecated/agent-reel/internal/reel"
	"os"
)

func main() {
	if err := reel.CLI(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		// areel attach passes the zellij client's exit status through silently.
		var status *reel.ExitStatus
		if errors.As(err, &status) {
			os.Exit(status.Code)
		}
		fmt.Fprintln(os.Stderr, "areel:", err)
		// 2 is a refused configuration; the packaged unit does not restart on it.
		os.Exit(reel.ExitCode(err))
	}
}
