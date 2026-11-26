package rex

// inputFilter strips the emulator's answers to the program's queries
// from the input of a Windows terminal. A ConPTY program reads input
// records, not bytes: a reply the terminal types — the device attributes
// (\x1b[?62;22c), the cursor's position (\x1b[8;59R), a mode report —
// reaches the shell as keystrokes, mangling or eating what the user
// typed next. No sequence a keyboard sends ends in c, R or y (keys end
// in A-F, H, ~ or u), so dropping those loses nothing real.
type inputFilter struct {
	hold []byte // a partial escape sequence split across chunks
}

// feed takes one chunk of input and returns what may reach the program.
// A chunk ending in a partial CSI sequence holds it for the next one.
func (f *inputFilter) feed(p []byte) []byte {
	in := append(f.hold, p...)
	f.hold = nil
	out := in[:0]
	for i := 0; i < len(in); {
		if in[i] != 0x1b || i+1 >= len(in) {
			if in[i] == 0x1b {
				// A lone escape at the chunk's end: the next byte decides.
				f.hold = append(f.hold, in[i:]...)
			} else {
				out = append(out, in[i])
			}
			i++
			continue
		}
		if in[i+1] != '[' {
			// Not a CSI sequence: escape and byte pass through.
			out = append(out, in[i], in[i+1])
			i += 2
			continue
		}
		// Scan the sequence: parameters and intermediates, then its final
		// byte.
		j := i + 2
		for j < len(in) && in[j] >= 0x20 && in[j] <= 0x3f {
			j++
		}
		if j >= len(in) {
			// The final byte comes in a later chunk.
			f.hold = append(f.hold, in[i:]...)
			break
		}
		switch final := in[j]; final {
		case 'c', 'R', 'y', 'n':
			// The emulator's answer to a query: never typed.
		default:
			if final >= 0x40 && final <= 0x7e {
				out = append(out, in[i:j+1]...)
			} else {
				// No CSI sequence after all: pass what was scanned and
				// reprocess the byte.
				out = append(out, in[i:j]...)
				i = j
				continue
			}
		}
		i = j + 1
	}
	// out aliases in; the hold must not share it.
	if len(f.hold) > 0 {
		f.hold = append([]byte(nil), f.hold...)
	}
	return append([]byte(nil), out...)
}

// flush returns a sequence held at a chunk's end, as when the pause
// after a lone escape — vim's, say — means it was never a sequence.
func (f *inputFilter) flush() []byte {
	b := f.hold
	f.hold = nil
	return b
}

// holding reports whether input waits for the next chunk.
func (f *inputFilter) holding() bool { return len(f.hold) > 0 }
