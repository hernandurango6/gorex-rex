//go:build !windows

package rex

// platformEndpoint is the machine's server on Unix: the socket of
// SocketPath, in XDG_RUNTIME_DIR or /run/user/<uid>.
func platformEndpoint() Endpoint {
	return Endpoint{Unix: SocketPath()}
}
