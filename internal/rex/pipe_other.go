//go:build !windows

package rex

import (
	"context"
	"fmt"
	"net"
)

// dialPipe connects to a named pipe; only Windows servers listen on one.
func dialPipe(ctx context.Context, path string) (net.Conn, error) {
	return nil, fmt.Errorf("rex: the pipe %s is a Windows endpoint, and this system has no named pipes", path)
}
