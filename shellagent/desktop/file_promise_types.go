package desktop

type filePromiseDescriptor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type filePromiseCommand struct {
	Type  string                  `json:"type"`
	Files []filePromiseDescriptor `json:"files,omitempty"`
	ID    string                  `json:"id,omitempty"`
	Error string                  `json:"error,omitempty"`
}

type filePromiseEvent struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Path string `json:"path"`
}
