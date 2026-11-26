package rex

import (
	"golang.org/x/sys/windows"
)

// platformEndpoint is the machine's server on Windows: the named pipe it
// listens on, named for the user's SID, as rex server status reports.
func platformEndpoint() Endpoint {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil || user.User.Sid == nil {
		return Endpoint{Pipe: `\\.\pipe\rex`}
	}
	return Endpoint{Pipe: `\\.\pipe\rex-` + user.User.Sid.String()}
}
