//go:build !android

package shellagent

import (
	"errors"
	"net"
)

// Desktop agents write bundles and shortcuts themselves and accept no
// platform host.
func nativePlatformUsesHostChannel() bool { return false }

// Desktop loopback is reachable only by programs on this machine, which the
// browser identity and settings guards already account for.
const loopbackSharedAcrossApps = false

func nativeVerifyPlatformPeer(net.Conn) error {
	return errors.New("this agent has no platform host")
}
