package desktop

type Command struct {
	ID      string `json:"id"`
	Service string `json:"service"`
	Method  string `json:"method"`
	Args    []any  `json:"args"`
}

type Reply struct {
	ID      string `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
	Service string `json:"service,omitempty"`
	Event   any    `json:"event,omitempty"`
}

type presentationCommand = Command
type presentationReply = Reply
