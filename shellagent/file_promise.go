package shellagent

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sync"
)

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
	Size  *int64                  `json:"size,omitempty"`
}

type filePromiseEvent struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Path string `json:"path"`
}

type filePromiseBridge struct {
	mu        sync.Mutex
	command   *exec.Cmd
	stdin     *json.Encoder
	onRequest func(id, path string)
}

func (s *Server) ensureFilePromiseBridge() (*filePromiseBridge, error) {
	s.promiseMu.Lock()
	defer s.promiseMu.Unlock()
	if s.promiseBridge != nil {
		return s.promiseBridge, nil
	}
	if !platformFilePromisesSupported() {
		return nil, errors.New("native file promises are not supported on this platform")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	command := exec.Command(executable, "file-promise-helper")
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	command.Stderr = os.Stderr
	if err = command.Start(); err != nil {
		return nil, err
	}
	bridge := &filePromiseBridge{command: command, stdin: json.NewEncoder(stdin)}
	s.promiseBridge = bridge
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var event filePromiseEvent
			if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "request" && bridge.onRequest != nil {
				bridge.onRequest(event.ID, event.Path)
			}
		}
		_ = command.Wait()
		s.promiseMu.Lock()
		if s.promiseBridge == bridge {
			s.promiseBridge = nil
		}
		s.promiseMu.Unlock()
	}()
	return bridge, nil
}

func (s *Server) discardFilePromiseBridge(bridge *filePromiseBridge) {
	s.promiseMu.Lock()
	if s.promiseBridge == bridge {
		s.promiseBridge = nil
	}
	s.promiseMu.Unlock()
	if bridge != nil && bridge.command != nil && bridge.command.Process != nil {
		_ = bridge.command.Process.Kill()
	}
}

func (b *filePromiseBridge) send(command filePromiseCommand) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stdin.Encode(command)
}

func (b *filePromiseBridge) publish(files []filePromiseDescriptor) error {
	return b.send(filePromiseCommand{Type: "publish", Files: files})
}

func (b *filePromiseBridge) complete(id string, err error) error {
	message := ""
	if err != nil {
		message = err.Error()
	}
	return b.send(filePromiseCommand{Type: "complete", ID: id, Error: message})
}
