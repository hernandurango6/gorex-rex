//go:build (linux || windows) && (amd64 || arm64)

package rex

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// ghostty binds the parts of libghostty-vt the terminal stream needs:
// decoding a snapshot into a terminal, and formatting that terminal back
// to the escape sequences that restore it in another. It loads the same
// library the terminal plugin does, from the path it's given.
var ghostty struct {
	once sync.Once
	err  error

	fnDecoderNew, fnDecoderFree, fnDecoderDecode, fnDecoderGet uintptr
	fnFormatAlloc, fnFormatterFree, fnFree, fnTerminalFree     uintptr
	decoderNew                                                 func(alloc uintptr, dec *uintptr, r ghosttyReader) int32
	formatterNew                                               func(alloc uintptr, out *uintptr, t uintptr, opts formatterOptions) int32
}

// ghosttyReader is a GhosttyReader: a read callback and its context.
type ghosttyReader struct {
	read     uintptr
	userdata uintptr
}

// formatterOptions is a GhosttyFormatterTerminalOptions.
type formatterOptions struct {
	size      uintptr
	emit      int32
	unwrap    bool
	trim      bool
	extra     formatterExtra
	selection uintptr
}

type formatterExtra struct {
	size                                                     uintptr
	palette, modes, scrollingRegion, tabstops, pwd, keyboard bool
	screen                                                   struct {
		size                                                          uintptr
		cursor, style, hyperlink, protection, kittyKeyboard, charsets bool
	}
}

// ghosttyCall calls a C function whose arguments are integers.
//
//go:uintptrescapes
func ghosttyCall(fn uintptr, args ...uintptr) uintptr {
	r, _, _ := purego.SyscallN(fn, args...)
	return r
}

// readers finds the feed of a snapshot decoder by its userdata.
var readers struct {
	sync.Mutex
	next uintptr
	m    map[uintptr]*snapshotFeed
}

// snapshotFeed gives the decoder the bytes of the stream, keeping what
// it delivered: bytes past the snapshot's end go back to the stream.
type snapshotFeed struct {
	pull func(p []byte) int // from the stream; -1 on error, 0 at end
	saw  []byte             // everything pull delivered
}

// mem makes an unsafe.Pointer of an address a callback received, whose
// uintptr keeps vet's pointer checks quiet.
func mem(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }

var cbRead = purego.NewCallback(func(id, buf, cap, outRead uintptr) uintptr {
	readers.Lock()
	f := readers.m[id]
	readers.Unlock()
	if f == nil {
		return 0
	}
	dst := unsafe.Slice((*byte)(mem(buf)), cap)
	n := f.pull(dst)
	if n < 0 {
		return 0
	}
	f.saw = append(f.saw, dst[:n]...)
	*(*uintptr)(mem(outRead)) = uintptr(n)
	return 1
})

// snapshotVT decodes one snapshot, read from pull, into the escape
// sequences that restore the terminal it holds in another one. Pull
// returns n>0 bytes, 0 at the end, or fails. The decoder may read past
// the snapshot's end: rest is what it pulled but didn't consume, the
// stream's first output.
func snapshotVT(pull func(p []byte) int) (vt, rest []byte, err error) {
	readers.Lock()
	readers.next++
	id := readers.next
	if readers.m == nil {
		readers.m = map[uintptr]*snapshotFeed{}
	}
	feed := &snapshotFeed{pull: pull}
	readers.m[id] = feed
	readers.Unlock()
	defer func() {
		readers.Lock()
		delete(readers.m, id)
		readers.Unlock()
	}()

	var dec uintptr
	if r := ghostty.decoderNew(0, &dec, ghosttyReader{read: cbRead, userdata: id}); r != 0 {
		return nil, nil, ghosttyErr("decoder_new", uintptr(r))
	}
	defer ghosttyCall(ghostty.fnDecoderFree, dec)
	var term uintptr
	if r := ghosttyCall(ghostty.fnDecoderDecode, dec, uintptr(unsafe.Pointer(&term))); r != 0 {
		return nil, nil, ghosttyErr("decoder_decode", r)
	}
	defer ghosttyCall(ghostty.fnTerminalFree, term)
	var used uintptr
	// GHOSTTY_SNAPSHOT_DECODER_DATA_SOURCE_OFFSET: the first byte after
	// the snapshot, which the reader may already have delivered.
	const dataSourceOffset = 2
	if r := ghosttyCall(ghostty.fnDecoderGet, dec, dataSourceOffset, uintptr(unsafe.Pointer(&used))); r != 0 {
		return nil, nil, ghosttyErr("decoder_get", r)
	}
	vt = formatVT(term)
	if vt == nil {
		return nil, nil, errors.New("rex: formatting the snapshot's terminal failed")
	}
	if int(used) < len(feed.saw) {
		rest = feed.saw[used:]
	}
	return vt, rest, nil
}

// formatVT renders the terminal as escape sequences: the scrollback and
// screen with styles, then modes, scrolling region, tab stops, working
// directory, keyboard flags and cursor, as the emulator's VT dump does.
func formatVT(term uintptr) []byte {
	opts := formatterOptions{size: unsafe.Sizeof(formatterOptions{}), emit: 1}
	x := &opts.extra
	x.size = unsafe.Sizeof(opts.extra)
	x.modes, x.scrollingRegion, x.tabstops, x.pwd, x.keyboard = true, true, true, true, true
	x.screen.size = unsafe.Sizeof(opts.extra.screen)
	x.screen.cursor, x.screen.style, x.screen.hyperlink, x.screen.protection, x.screen.kittyKeyboard, x.screen.charsets = true, true, true, true, true, true
	var f uintptr
	if ghostty.formatterNew(0, &f, term, opts) != 0 {
		return nil
	}
	defer ghosttyCall(ghostty.fnFormatterFree, f)
	var p, n uintptr
	if r := ghosttyCall(ghostty.fnFormatAlloc, f, 0, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&n))); r != 0 {
		return nil
	}
	b := make([]byte, n)
	copy(b, unsafe.Slice((*byte)(mem(p)), n))
	if p != 0 {
		ghosttyCall(ghostty.fnFree, 0, p, n)
	}
	return b
}

func ghosttyErr(op string, r uintptr) error {
	return fmt.Errorf("rex: libghostty-vt %s: error %d", op, int32(r))
}

// snapshotSupported reports whether the platform can decode snapshots.
const snapshotSupported = true
