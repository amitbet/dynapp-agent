package shellagent

import (
	"context"
	"errors"
	"strings"

	"github.com/amitbet/dynapp-agent/shellagent/netfiles"
)

func (s *Server) handleNetworkFileRPC(ctx context.Context, socket protocolSocket, request message) (any, error) {
	if len(request.Args) < 1 {
		return nil, errors.New("network connection is required")
	}
	if request.Method == "copyToLocal" && !request.allows("fs.writeBase64") {
		return nil, errors.New("copyToLocal requires fs.writeBase64")
	}
	if request.Method == "copyFromLocal" && !request.allows("fs.readBase64") {
		return nil, errors.New("copyFromLocal requires fs.readBase64")
	}
	if connection, _ := request.Args[0].(map[string]any); strings.TrimSpace(stringValue(connection["identityFile"])) != "" && !request.allows("fs.readText") {
		return nil, errors.New("identityFile requires fs.readText")
	}
	return netfiles.HandleRPC(ctx, s.StateDir, request.Service, request.Method, request.Args, func(event map[string]any) {
		send(socket, ctx, map[string]any{"type": "rpc-event", "service": request.Service, "event": event})
	})
}
