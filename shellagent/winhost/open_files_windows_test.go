//go:build windows

package winhost

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestQueueOpenFilesWaitsForAcknowledgement(t *testing.T) {
	for _, rejection := range []string{"", "Allow opening external files first"} {
		t.Run(rejection, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			done := make(chan error, 1)
			go func() {
				done <- queueOpenFiles(&frameConn{conn: client, reader: bufio.NewReader(client)}, []string{`notes with spaces.txt`, `-draft.md`})
			}()
			peer := &frameConn{conn: server, reader: bufio.NewReader(server)}
			_, payload, err := peer.read()
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				Type  string
				Paths []string
			}
			if err := json.Unmarshal(payload, &request); err != nil {
				t.Fatal(err)
			}
			if request.Type != "native-host-open-files" || len(request.Paths) != 2 || !filepath.IsAbs(request.Paths[0]) || !strings.HasSuffix(request.Paths[0], "notes with spaces.txt") {
				t.Fatalf("request = %+v", request)
			}
			select {
			case err := <-done:
				t.Fatalf("returned before acknowledgement: %v", err)
			default:
			}
			if err := peer.writeJSON(map[string]any{"type": "native-host-open-files-result", "error": rejection}); err != nil {
				t.Fatal(err)
			}
			err = <-done
			if rejection == "" && err != nil || rejection != "" && (err == nil || err.Error() != rejection) {
				t.Fatalf("result = %v", err)
			}
		})
	}
}
