//go:build windows && (amd64 || arm64)

package rex

import (
	"fmt"
	"syscall"

	"github.com/ebitengine/purego"
)

// ghosttyLoad opens the library at path and binds the symbols, as the
// terminal plugin does on Windows: LoadLibrary, since purego's dlopen
// is Unix's.
func ghosttyLoad(path string) error {
	ghostty.once.Do(func() {
		h, err := syscall.LoadLibrary(path)
		if err != nil {
			ghostty.err = err
			return
		}
		for _, s := range []struct {
			name string
			addr *uintptr
		}{
			{"ghostty_snapshot_decoder_free", &ghostty.fnDecoderFree},
			{"ghostty_snapshot_decoder_decode", &ghostty.fnDecoderDecode},
			{"ghostty_snapshot_decoder_get", &ghostty.fnDecoderGet},
			{"ghostty_formatter_format_alloc", &ghostty.fnFormatAlloc},
			{"ghostty_formatter_free", &ghostty.fnFormatterFree},
			{"ghostty_free", &ghostty.fnFree},
			{"ghostty_terminal_free", &ghostty.fnTerminalFree},
		} {
			a, err := syscall.GetProcAddress(syscall.Handle(h), s.name)
			if err != nil {
				ghostty.err = fmt.Errorf("%s: %w", s.name, err)
				return
			}
			*s.addr = a
		}
		fn, err := syscall.GetProcAddress(syscall.Handle(h), "ghostty_snapshot_decoder_new")
		if err != nil {
			ghostty.err = err
			return
		}
		purego.RegisterFunc(&ghostty.decoderNew, fn)
		if fn, err = syscall.GetProcAddress(syscall.Handle(h), "ghostty_formatter_terminal_new"); err != nil {
			ghostty.err = err
			return
		}
		purego.RegisterFunc(&ghostty.formatterNew, fn)
	})
	return ghostty.err
}
