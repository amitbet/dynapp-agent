package desktop

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

func RunHelper(address, token string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" || len(token) != 64 {
		return errors.New("invalid presentation channel")
	}
	conn, err := net.DialTimeout("tcp4", address, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = fmt.Fprintln(conn, token); err != nil {
		return err
	}
	var mu sync.Mutex
	emit := func(reply presentationReply) {
		mu.Lock()
		defer mu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if json.NewEncoder(conn).Encode(reply) != nil {
			conn.Close()
		}
	}
	commands := make(chan presentationCommand)
	go func() {
		defer close(commands)
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			var command presentationCommand
			if json.Unmarshal(scanner.Bytes(), &command) != nil {
				break
			}
			commands <- command
		}
	}()
	return runNativePresentation(commands, emit)
}
