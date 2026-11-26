package rex

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// dialPipe connects to the server's named pipe. go-winio speaks the
// overlapped, message-aware I/O Windows's pipes need, which net doesn't;
// the Identification impersonation level lets the server read the
// client's token — its SID authenticates it — without letting the server
// act as it, which DialPipe's Anonymous default forbids.
func dialPipe(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeAccessImpLevel(ctx, path,
		windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelIdentification)
}
