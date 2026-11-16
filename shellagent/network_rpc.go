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
	if request.Method == "copyToLocal" {
		if err := s.requireCapability(ctx, request, "fs.writeBase64", "copyToLocal requires fs.writeBase64"); err != nil {
			return nil, err
		}
	}
	if request.Method == "copyFromLocal" {
		if err := s.requireCapability(ctx, request, "fs.readBase64", "copyFromLocal requires fs.readBase64"); err != nil {
			return nil, err
		}
	}
	if connection, _ := request.Args[0].(map[string]any); strings.TrimSpace(stringValue(connection["identityFile"])) != "" {
		if err := s.requireCapability(ctx, request, "fs.readText", "identityFile requires fs.readText"); err != nil {
			return nil, err
		}
	}
	return netfiles.HandleRPC(ctx, s.StateDir, request.Service, request.Method, request.Args, func(event map[string]any) {
		send(socket, ctx, map[string]any{"type": "rpc-event", "service": request.Service, "event": event})
	})
}
