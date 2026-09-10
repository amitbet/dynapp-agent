package agentutil

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"os/user"
	"strings"
)

func HomeDir() string {
	if current, err := user.Current(); err == nil && current.HomeDir != "" {
		return current.HomeDir
	}
	home, _ := os.UserHomeDir()
	return home
}

func SafeName(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

func RandomToken(size int) string {
	raw := make([]byte, size)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}
