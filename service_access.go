package main

import (
	"regexp"
	"strings"
)

var interactiveACE = regexp.MustCompile(`\(A;;([A-Z]*);;;IU\)`)

// withInteractiveStartRight adds RP (SERVICE_START) to the interactive-user
// allow ACE of a service security descriptor, or adds such an ACE, leaving
// every other entry untouched. It is a pure function so it can be tested on
// any platform.
func withInteractiveStartRight(sddl string) string {
	if sddl == "" {
		return sddl
	}
	if match := interactiveACE.FindStringSubmatchIndex(sddl); match != nil {
		rights := sddl[match[2]:match[3]]
		for index := 0; index+1 < len(rights); index += 2 {
			if rights[index:index+2] == "RP" {
				return sddl
			}
		}
		return sddl[:match[3]] + "RP" + sddl[match[3]:]
	}
	ace := "(A;;LCRPLO;;;IU)"
	if start := strings.Index(sddl, "D:"); start >= 0 {
		end := len(sddl)
		if sacl := strings.Index(sddl[start:], "S:"); sacl >= 0 {
			end = start + sacl
		}
		return sddl[:end] + ace + sddl[end:]
	}
	return sddl
}
