package shellagent

import (
	"context"

	"github.com/zalando/go-keyring"
)

// Services a platform host provides where the desktop tools do not exist.
// They stay nil on desktops, which use the system tools directly.
var (
	platformOpenPath           func(ctx context.Context, path string, reveal bool) error
	platformWriteClipboardText func(text string) error
)

// Secret values are written to the OS credential store; the agent never reads
// them back through RPC.
var (
	secretStoreSet    = keyring.Set
	secretStoreDelete = keyring.Delete
)
