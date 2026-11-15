//go:build !windows

package shellagent

import (
	"net"
	"time"
)

func dialAutomation(endpoint string) (net.Conn, error) {
	return net.DialTimeout("unix", endpoint, 2*time.Second)
}
