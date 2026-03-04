//go:build windows

package lifecycle

import "os"

func TerminationSignals(includeInterrupt bool) []os.Signal {
	if includeInterrupt {
		return []os.Signal{os.Interrupt}
	}
	return []os.Signal{}
}
