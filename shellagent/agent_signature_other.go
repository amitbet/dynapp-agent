//go:build !darwin

package shellagent

func verifyDownloadedAgentSignature(string) error { return nil }
