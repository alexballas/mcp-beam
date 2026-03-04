//go:build !windows

package lifecycle

import (
	"os"
	"syscall"
)

func TerminationSignals(includeInterrupt bool) []os.Signal {
	signals := []os.Signal{syscall.SIGTERM}
	if includeInterrupt {
		signals = append(signals, os.Interrupt)
	}
	return signals
}
