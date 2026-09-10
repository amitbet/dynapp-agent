package shellagent

import (
	"context"
	"errors"
)

func rpcResult(socket protocolSocket, ctx context.Context, request message, result any) {
	send(socket, ctx, map[string]any{"type": "rpc-result", "id": request.ID, "service": request.Service, "result": result})
}
func rpcError(socket protocolSocket, ctx context.Context, request message, err error) {
	send(socket, ctx, map[string]any{"type": "rpc-error", "id": request.ID, "service": request.Service, "error": err.Error()})
}
func (s *Server) handleRPC(socket protocolSocket, ctx context.Context, agents *agentService, request message, body []byte) {
	var result any
	var err error
	switch request.Service {
	case "agent":
		result, err = agents.handle(ctx, request)
	case "calendar":
		result, err = s.handleCalendarRPC(ctx, request)
	case "fileSearch":
		result, err = s.handleFileSearchRPC(ctx, request)
	case "secrets":
		result, err = s.handleSecretsRPC(ctx, request)
	case "ftp", "sftp":
		result, err = s.handleNetworkFileRPC(ctx, socket, request)
	case "externalOpen":
		result, err = s.handleExternalOpenRPC(ctx, request)
	case "associations":
		result, err = s.handleAssociationsRPC(request)
	case "fileImport":
		result, err = s.handleFileImportRPC(request)
	case "clipboard":
		result, err = s.handleClipboardRPCWithSocket(socket, request, body)
	case "lan":
		result, err = s.handleLANRPC(request)
	case "sessions":
		result, err = s.handleSessionsRPC(request)
	case "system":
		result, err = s.handleSystemRPC(ctx, request)
	case "http":
		result, err = s.handleHTTPRPC(ctx, request)
	case "apps":
		result, err = s.handleAppsRPC(ctx, request)
	case "permissions":
		result, err = s.handlePermissionsRPC(socket, request)
	default:
		err = errors.New("unsupported Shell service")
	}
	if err != nil {
		rpcError(socket, ctx, request, err)
	} else {
		rpcResult(socket, ctx, request, result)
	}
}
