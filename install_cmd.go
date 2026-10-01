package main

import (
	"regexp"
	"strings"
)

// defaultAgentAddress is where install-user waits for the new agent.
const defaultAgentAddress = "127.0.0.1:9011"

var stateDirArgument = regexp.MustCompile(`--state-dir\s+(?:"([^"]+)"|(\S+))`)

// stateDirFromCommandLine reads --state-dir from a service command line.
func stateDirFromCommandLine(commandLine string) string {
	match := stateDirArgument.FindStringSubmatch(commandLine)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1] + match[2])
}
