//go:build !windows

package lifecycle

import (
	"os"
	"syscall"
)

func TerminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
