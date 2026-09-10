//go:build !linux

package desktop

import "context"

func StartRepair(string) context.CancelFunc { return nil }
