//go:build !linux || android

package shellagent

// updateNativeHostBinary: only Linux ships the app host as its own binary.
func (s *Server) updateNativeHostBinary() {}
