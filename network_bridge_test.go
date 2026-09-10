package shellagent

import (
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestTransientUDPReadErrors(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("udp read: %w", syscall.ECONNREFUSED),
		fmt.Errorf("udp read: %w", syscall.ECONNRESET),
		fmt.Errorf("udp read: %w", syscall.Errno(10054)),
		fmt.Errorf("udp read: %w", syscall.Errno(10061)),
	} {
		if !transientUDPReadError(err) {
			t.Fatalf("%v was not treated as transient", err)
		}
	}
	if transientUDPReadError(fmt.Errorf("permanent failure")) {
		t.Fatal("unrelated error was treated as transient")
	}
}

func TestDialUDPBridgeBindsRequestedLocalPort(t *testing.T) {
	target, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	reservation, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	localPort := reservation.LocalAddr().(*net.UDPAddr).Port
	_ = reservation.Close()

	connection, err := dialUDPBridge("127.0.0.1", target.LocalAddr().(*net.UDPAddr).Port, localPort)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if got := connection.LocalAddr().(*net.UDPAddr).Port; got != localPort {
		t.Fatalf("local port = %d, want %d", got, localPort)
	}
}

func TestBridgeSetTracksActiveBridges(t *testing.T) {
	var active atomic.Int64
	bridges := newBridgeSet(&active)
	firstClient, firstAgent := net.Pipe()
	secondClient, secondAgent := net.Pipe()
	defer firstClient.Close()
	defer secondClient.Close()
	bridges.put("tcp_1", firstAgent, nil)
	bridges.put("udp_2", secondAgent, nil)
	if got := active.Load(); got != 2 {
		t.Fatalf("active bridge count = %d, want 2", got)
	}
	_ = bridges.take("tcp_1").Close()
	if got := active.Load(); got != 1 {
		t.Fatalf("active bridge count after take = %d, want 1", got)
	}
	bridges.closeAll()
	if got := active.Load(); got != 0 {
		t.Fatalf("active bridge count after closeAll = %d, want 0", got)
	}
}
