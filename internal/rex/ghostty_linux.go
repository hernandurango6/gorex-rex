//go:build linux && (amd64 || arm64)

package rex

import (
	"fmt"

	"github.com/ebitengine/purego"
)

// ghosttyLoad opens the library at path and binds the symbols.
func ghosttyLoad(path string) error {
	ghostty.once.Do(func() {
		h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
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
			if *s.addr, err = purego.Dlsym(h, s.name); err != nil {
				ghostty.err = fmt.Errorf("%s: %w", s.name, err)
				return
			}
		}
		var fn uintptr
		if fn, err = purego.Dlsym(h, "ghostty_snapshot_decoder_new"); err != nil {
			ghostty.err = err
			return
		}
		purego.RegisterFunc(&ghostty.decoderNew, fn)
		if fn, err = purego.Dlsym(h, "ghostty_formatter_terminal_new"); err != nil {
			ghostty.err = err
			return
		}
		purego.RegisterFunc(&ghostty.formatterNew, fn)
	})
	return ghostty.err
}
