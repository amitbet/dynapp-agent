package proc

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"

	ptylib "github.com/aymanbagabas/go-pty"
)

var execEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func execEnvironment(extra map[string]any) ([]string, error) {
	if extra == nil {
		return nil, nil
	}
	if len(extra) > MaxEnvVars {
		return nil, errors.New("Executable request is invalid")
	}
	env := append([]string{}, os.Environ()...)
	index := make(map[string]int, len(env))
	for i, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			index[name] = i
		}
	}
	set := func(name, value string) {
		entry := name + "=" + value
		if i, ok := index[name]; ok {
			env[i] = entry
			return
		}
		index[name] = len(env)
		env = append(env, entry)
	}
	unset := func(name string) {
		i, ok := index[name]
		if !ok {
			return
		}
		last := len(env) - 1
		moved := env[last]
		env[i] = moved
		env = env[:last]
		delete(index, name)
		if i < last {
			movedName, _, _ := strings.Cut(moved, "=")
			index[movedName] = i
		}
	}
	for name, raw := range extra {
		if !execEnvNamePattern.MatchString(name) {
			return nil, errors.New("Executable request is invalid")
		}
		if raw == nil {
			unset(name)
			continue
		}
		text, ok := raw.(string)
		if !ok || len(text) > MaxEnvBytes {
			return nil, errors.New("Executable request is invalid")
		}
		set(name, text)
	}
	return env, nil
}

func ptyEnvironment(extra map[string]any, term string) ([]string, error) {
	merged := make(map[string]any, len(extra)+1)
	for name, value := range extra {
		merged[name] = value
	}
	if _, exists := merged["TERM"]; !exists {
		name := strings.TrimSpace(term)
		if name == "" {
			name = "xterm-256color"
		}
		merged["TERM"] = name
	}
	return execEnvironment(merged)
}

func applyExecEnv(command *exec.Cmd, extra map[string]any) error {
	env, err := execEnvironment(extra)
	if err == nil {
		command.Env = env
	}
	return err
}

// Set is the remote-protocol equivalent of Electron's managed process
// service. Handles are scoped to a single authenticated socket, so another
// app, browser session, or relay tenant can never address them.
type Set struct {
	send    Sender
	ctx     context.Context
	mu      sync.Mutex
	next    uint64
	records map[string]*managedProcess
}
type managedProcess struct {
	process       *os.Process
	wait          func() error
	stdin         io.WriteCloser
	terminal      ptylib.Pty
	cancel        context.CancelFunc
	closeTerminal func()
}

type execOutputBudget struct {
	mu        sync.Mutex
	remaining int
}

func (budget *execOutputBudget) take(count int) int {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.remaining <= 0 {
		return 0
	}
	if count > budget.remaining {
		count = budget.remaining
	}
	budget.remaining -= count
	return count
}

func NewSet(send Sender, ctx context.Context) *Set {
	return &Set{send: send, ctx: ctx, records: map[string]*managedProcess{}}
}
func (p *Set) Start(request Request, args []string) {
	if len(request.File) > MaxArgumentBytes || len(request.Cwd) > MaxArgumentBytes || len(args) > MaxArgs {
		p.send(map[string]any{"type": "exec-error", "id": request.ID, "error": "Executable request is invalid"})
		return
	}
	for _, arg := range args {
		if len(arg) > MaxArgumentBytes {
			p.send(map[string]any{"type": "exec-error", "id": request.ID, "error": "Executable request is invalid"})
			return
		}
	}
	p.mu.Lock()
	p.next++
	id := "proc_" + strconv.FormatUint(p.next, 36)
	p.mu.Unlock()
	execContext, cancel := context.WithCancel(p.ctx)
	var process *os.Process
	var wait func() error
	var exitStatus func() int
	var stdin io.WriteCloser
	var terminal ptylib.Pty
	var err error
	var outputs []struct {
		stream string
		reader io.Reader
	}
	if request.Mode == "pty" {
		terminal, err = ptylib.New()
		if err == nil {
			err = terminal.Resize(normalizeTerminalSize(request.Cols, 80, 2, 1000), normalizeTerminalSize(request.Rows, 24, 1, 1000))
		}
		if err != nil {
			cancel()
			p.send(map[string]any{"type": "exec-error", "id": request.ID, "error": err.Error()})
			return
		}
		command := terminal.CommandContext(execContext, request.File, args...)
		command.Dir = request.Cwd
		command.Env, err = ptyEnvironment(request.Env, request.Term)
		if err == nil {
			err = command.Start()
		}
		if err != nil {
			_ = terminal.Close()
			cancel()
			p.send(map[string]any{"type": "exec-error", "id": request.ID, "error": err.Error()})
			return
		}
		// Drop the parent's slave fd so child exit EOFs the master. Leaving it
		// open forces Close() to unblock readers and can discard the last output.
		closeParentPtySlave(terminal)
		process, wait, stdin = command.Process, command.Wait, terminal
		exitStatus = func() int {
			if command.ProcessState == nil {
				return -1
			}
			return command.ProcessState.ExitCode()
		}
		outputs = append(outputs, struct {
			stream string
			reader io.Reader
		}{"stdout", terminal})
	} else {
		command := exec.CommandContext(execContext, request.File, args...)
		command.Dir = request.Cwd
		if err = applyExecEnv(command, request.Env); err == nil {
			var stdout, stderr io.ReadCloser
			stdout, err = command.StdoutPipe()
			if err == nil {
				stderr, err = command.StderrPipe()
			}
			if err == nil && request.Stdin != "ignore" {
				stdin, err = command.StdinPipe()
			}
			if err == nil {
				err = command.Start()
			}
			if err == nil {
				process, wait = command.Process, command.Wait
				exitStatus = func() int {
					if command.ProcessState == nil {
						return -1
					}
					return command.ProcessState.ExitCode()
				}
				outputs = append(outputs, struct {
					stream string
					reader io.Reader
				}{"stdout", stdout}, struct {
					stream string
					reader io.Reader
				}{"stderr", stderr})
			}
		}
		if err != nil {
			cancel()
			p.send(map[string]any{"type": "exec-error", "id": request.ID, "error": err.Error()})
			return
		}
	}
	// Process exit and socket teardown may race. The PTY library's Close is
	// not concurrent-safe, so both paths share a single close operation.
	closeTerminal := sync.OnceFunc(func() {
		if terminal != nil {
			_ = terminal.Close()
		}
	})
	p.mu.Lock()
	p.records[id] = &managedProcess{process: process, wait: wait, stdin: stdin, terminal: terminal, cancel: cancel, closeTerminal: closeTerminal}
	p.mu.Unlock()
	p.send(map[string]any{"type": "exec-start", "id": request.ID, "processId": id})
	budget := &execOutputBudget{remaining: MaxOutputBytes}
	var outputWG sync.WaitGroup
	for _, output := range outputs {
		outputWG.Add(1)
		go func(stream string, reader io.Reader) {
			defer outputWG.Done()
			p.copyOutput(id, stream, reader, budget)
		}(output.stream, output.reader)
	}
	go func() {
		defer cancel()
		defer closeTerminal()
		err := wait()
		outputWG.Wait()
		code := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else if execContext.Err() == context.DeadlineExceeded {
				p.send(map[string]any{"type": "process-event", "processId": id, "event": "error", "message": "Remote command timed out"})
			} else {
				p.send(map[string]any{"type": "process-event", "processId": id, "event": "error", "message": err.Error()})
			}
		}
		if status := exitStatus(); status >= 0 {
			code = status
		}
		p.send(map[string]any{"type": "exec-exit", "id": request.ID, "processId": id, "code": code, "signal": nil})
		p.mu.Lock()
		delete(p.records, id)
		p.mu.Unlock()
	}()
}

func closeParentPtySlave(terminal ptylib.Pty) {
	unix, ok := terminal.(ptylib.UnixPty)
	if !ok {
		return
	}
	if slave := unix.Slave(); slave != nil {
		_ = slave.Close()
	}
}

func normalizeTerminalSize(value, fallback, minimum, maximum int) int {
	if value < minimum || value > maximum {
		return fallback
	}
	return value
}

func (p *Set) copyOutput(id, stream string, reader io.Reader, budget *execOutputBudget) {
	buffer := make([]byte, 32*1024)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			if allowed := budget.take(count); allowed > 0 {
				p.send(map[string]any{"type": "exec-output", "processId": id, "stream": stream, "data": string(buffer[:allowed])})
			}
		}
		if err != nil {
			return
		}
	}
}
func (p *Set) Handle(request Request) {
	p.mu.Lock()
	record := p.records[request.ProcessID]
	p.mu.Unlock()
	if record == nil {
		p.send(map[string]any{"type": "process-error", "id": request.ID, "processId": request.ProcessID, "error": "Unknown process handle"})
		return
	}
	switch request.Action {
	case "status":
		p.send(map[string]any{"type": "process-result", "id": request.ID, "processId": request.ProcessID, "result": map[string]any{"processId": request.ProcessID, "status": "running", "pid": record.process.Pid}})
	case "write":
		if record.stdin == nil {
			p.send(map[string]any{"type": "process-error", "id": request.ID, "processId": request.ProcessID, "error": "Process stdin is not writable"})
			return
		}
		if len(request.Data) > 1_000_000 {
			p.send(map[string]any{"type": "process-error", "id": request.ID, "processId": request.ProcessID, "error": "Process input is too large"})
			return
		}
		_, err := io.WriteString(record.stdin, request.Data)
		if err != nil {
			p.send(map[string]any{"type": "process-error", "id": request.ID, "processId": request.ProcessID, "error": err.Error()})
			return
		}
		p.send(map[string]any{"type": "process-result", "id": request.ID, "processId": request.ProcessID, "result": true})
	case "resize":
		if record.terminal == nil {
			p.send(map[string]any{"type": "process-result", "id": request.ID, "processId": request.ProcessID, "result": false})
			return
		}
		err := record.terminal.Resize(normalizeTerminalSize(request.Cols, 80, 2, 1000), normalizeTerminalSize(request.Rows, 24, 1, 1000))
		if err != nil {
			p.send(map[string]any{"type": "process-error", "id": request.ID, "processId": request.ProcessID, "error": err.Error()})
			return
		}
		p.send(map[string]any{"type": "process-result", "id": request.ID, "processId": request.ProcessID, "result": true})
	case "signal", "close":
		signal := strings.ToUpper(request.Signal)
		if request.Action == "close" || signal == "" {
			signal = "SIGTERM"
		}
		if signal != "SIGINT" && signal != "SIGTERM" && signal != "SIGKILL" && signal != "SIGHUP" {
			p.send(map[string]any{"type": "process-error", "id": request.ID, "processId": request.ProcessID, "error": "Unsupported process signal"})
			return
		}
		err := record.process.Signal(parseSignal(signal))
		if err != nil {
			p.send(map[string]any{"type": "process-error", "id": request.ID, "processId": request.ProcessID, "error": err.Error()})
			return
		}
		p.send(map[string]any{"type": "process-result", "id": request.ID, "processId": request.ProcessID, "result": true})
	default:
		p.send(map[string]any{"type": "process-error", "id": request.ID, "processId": request.ProcessID, "error": "Unsupported process request"})
	}
}
func (p *Set) CloseAll() {
	p.mu.Lock()
	records := p.records
	p.records = map[string]*managedProcess{}
	p.mu.Unlock()
	for _, record := range records {
		record.cancel()
		_ = record.process.Kill()
		if record.closeTerminal != nil {
			record.closeTerminal()
		}
	}
}
