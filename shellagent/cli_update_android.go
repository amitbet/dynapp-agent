//go:build android

package shellagent

import (
	"context"
	"errors"
)

func findDynappCLIs() ([]cliInstall, func()) { return nil, func() {} }

func replaceDynappCLI(context.Context, string, string, cliInstall) error {
	return errors.New("not on Android")
}
