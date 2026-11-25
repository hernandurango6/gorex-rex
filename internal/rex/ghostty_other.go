//go:build !(linux && (amd64 || arm64))

package rex

// snapshotSupported reports whether the platform can decode snapshots:
// only the libghostty-vt Linux binding is written for now.
const snapshotSupported = false

// ghosttyLoad opens the library; off Linux it isn't bound.
func ghosttyLoad(path string) error { return nil }

func snapshotVT(pull func(p []byte) int) (vt, rest []byte, err error) { return nil, nil, nil }
