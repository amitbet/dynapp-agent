//go:build windows

package shellagent

import (
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// dialAutomation tries the per-user agent's pipe, then the machine
// service's pipe (an agent running as LocalSystem).
func dialAutomation(endpoint string) (net.Conn, error) {
	timeout := 2 * time.Second
	conn, err := winio.DialPipe(endpoint, &timeout)
	if err == nil || endpoint == `\\.\pipe\dynapp-automation` {
		return conn, err
	}
	if service, serviceErr := winio.DialPipe(`\\.\pipe\dynapp-automation`, &timeout); serviceErr == nil {
		return service, nil
	}
	return nil, err
}
