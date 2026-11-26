//go:build !((linux || windows) && (amd64 || arm64))

package rex

// snapshotSupported reports whether the platform can decode snapshots:
// the libghostty-vt binding is written for Linux and Windows.
const snapshotSupported = false

// ghosttyLoad opens the library; off those systems it isn't bound.
func ghosttyLoad(path string) error { return nil }

func snapshotVT(pull func(p []byte) int) (vt, rest []byte, err error) { return nil, nil, nil }
