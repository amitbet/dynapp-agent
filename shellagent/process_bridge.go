package shellagent

import (
	"context"

	"github.com/amitbet/dynapp-agent/shellagent/proc"
)

func processSender(socket protocolSocket, ctx context.Context) proc.Sender {
	return func(value any) { send(socket, ctx, value) }
}

func processRequest(request message) proc.Request {
	return proc.Request{
		ID: request.ID, File: request.File, Cwd: request.Cwd, Mode: request.Mode, Term: request.Term,
		ProcessID: request.ProcessID, Action: request.Action, Data: request.Data, Signal: request.Signal, Stdin: request.Stdin,
		Env: request.Env, Cols: request.Cols, Rows: request.Rows,
	}
}
