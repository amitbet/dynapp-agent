package shellagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type dynerAccountFile struct {
	Token   string `json:"token"`
	Account struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
	} `json:"account"`
}

func DefaultDynerAuthPath() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "DynApp", "dyner", "auth.json"), nil
}

func LoadDynerAccountToken(authPath string) (string, error) {
	if strings.TrimSpace(authPath) == "" {
		resolved, err := DefaultDynerAuthPath()
		if err != nil {
			return "", err
		}
		authPath = resolved
	}
	data, err := os.ReadFile(authPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var file dynerAccountFile
	if err := json.Unmarshal(data, &file); err != nil {
		return "", err
	}
	token := strings.TrimSpace(file.Token)
	if token == "" || file.Account.User.Email == "" {
		return "", nil
	}
	return token, nil
}

func ResolveAccountToken(explicit, authPath string) (string, error) {
	if token := strings.TrimSpace(explicit); token != "" {
		return token, nil
	}
	if token := strings.TrimSpace(os.Getenv("DYNAPP_DYNER_TOKEN")); token != "" {
		return token, nil
	}
	return LoadDynerAccountToken(authPath)
}
