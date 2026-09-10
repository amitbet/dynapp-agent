package proc

import (
	"bytes"
	"os/exec"
	"time"
)

const (
	MaxArgs          = 128
	MaxArgumentBytes = 4096
	MaxEnvVars       = 64
	MaxEnvBytes      = 16 * 1024
	MaxOutputBytes   = 8 * 1024 * 1024
	MaxDuration      = 60 * time.Second
)

type Sender func(value any)

type Request struct {
	ID, File, Cwd, Mode, Term, ProcessID, Action, Data, Signal, Stdin string
	Env                                                               map[string]any
	Cols, Rows                                                        int
}

type BoundedBuffer struct {
	bytes.Buffer
	Limit int
}

func (buffer *BoundedBuffer) Write(value []byte) (int, error) {
	if remaining := buffer.Limit - buffer.Len(); remaining > 0 {
		if len(value) > remaining {
			_, _ = buffer.Buffer.Write(value[:remaining])
		} else {
			_, _ = buffer.Buffer.Write(value)
		}
	}
	return len(value), nil
}

func ApplyEnv(command *exec.Cmd, extra map[string]any) error {
	return applyExecEnv(command, extra)
}
