//go:build windows

package lifecycle

import "os"

func TerminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
