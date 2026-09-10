package shellagent

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func (s *Server) handleAssociationsRPC(request message) (any, error) {
	appID, err := associationAppID(request.AppID)
	if err != nil {
		return nil, err
	}
	switch request.Method {
	case "getState":
		state, err := AssociationState(appID)
		if err != nil {
			return nil, err
		}
		desired := uniqueExtensions(stringSliceLoose(objectArg(request.Args, 0)["extensions"]))
		if len(desired) > 0 {
			applied, _ := state["appliedExtensions"].([]string)
			appliedSet := map[string]bool{}
			for _, extension := range applied {
				appliedSet[extension] = true
			}
			count := 0
			for _, extension := range desired {
				if appliedSet[extension] {
					count++
				}
			}
			state["extensions"] = desired
			state["applied"] = count == len(desired)
			state["partiallyApplied"] = count > 0 && count < len(desired)
		}
		return state, nil
	case "remove":
		return true, RemoveAssociations(appID)
	case "apply":
		options := objectArg(request.Args, 0)
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		executable, err = persistAssociationExecutable(s.StateDir, executable)
		if err != nil {
			return nil, err
		}
		extensions := []string{}
		for _, value := range stringSliceLoose(options["extensions"]) {
			value = strings.TrimPrefix(value, ".")
			if value != "" {
				extensions = append(extensions, value)
			}
		}
		err = InstallAssociations(AssociationOptions{AppID: appID, Name: stringValue(options["name"]), URL: stringValue(options["url"]), Executable: executable, Extensions: extensions})
		return err == nil, err
	default:
		return nil, errors.New("unsupported file-association method")
	}
}

func persistAssociationExecutable(stateDir, source string) (string, error) {
	if stateDir == "" {
		var err error
		stateDir, err = DefaultStateDir()
		if err != nil {
			return "", err
		}
	}
	name := "dynapp-shell-agent"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(stateDir, "os-integration", "file-associations", name)
	if filepath.Clean(source) == filepath.Clean(target) {
		return target, nil
	}
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".agent-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err = io.Copy(temporary, input); err == nil {
		err = temporary.Chmod(0o700)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return "", err
	}
	return target, nil
}

func associationAppID(value string) (string, error) {
	if validStoreID(value) {
		return storeSlug(value), nil
	}
	if value == "" {
		return "", errors.New("app id is required")
	}
	if safeName(value) != value {
		return "", errors.New("app id is invalid")
	}
	return value, nil
}
func stringSliceLoose(value any) []string {
	raw, _ := value.([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		out = append(out, stringValue(item))
	}
	return out
}
