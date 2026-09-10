//go:build windows

package proc

import "os"

// Windows has no portable POSIX signal delivery for arbitrary child processes.
// os.Kill is the safe common denominator for close/interrupt requests.
func parseSignal(string) os.Signal { return os.Kill }
