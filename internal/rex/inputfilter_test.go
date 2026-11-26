package rex

import (
	"bytes"
	"testing"
)

func TestInputFilter(t *testing.T) {
	cases := []struct {
		name string
		in   [][]byte // chunks fed in order
		want []byte   // everything that comes out, no flush
	}{
		{"plain text", [][]byte{[]byte("exit\r")}, []byte("exit\r")},
		{"da reply dropped", [][]byte{[]byte("\x1b[?62;22c" + "exit\r")}, []byte("exit\r")},
		{"cpr reply dropped", [][]byte{[]byte("\x1b[8;59Rcd\r")}, []byte("cd\r")},
		{"dsr reply dropped", [][]byte{[]byte("\x1b[0nx")}, []byte("x")},
		{"reply alone", [][]byte{[]byte("\x1b[?1;2c")}, nil},
		{"reply split across chunks", [][]byte{[]byte("\x1b[?62;2"), []byte("2c" + "cd\r")}, []byte("cd\r")},
		{"arrows pass", [][]byte{[]byte("\x1b[A\x1b[B\x1b[H\x1b[F")}, []byte("\x1b[A\x1b[B\x1b[H\x1b[F")},
		{"tilde keys pass", [][]byte{[]byte("\x1b[3~\x1b[15~")}, []byte("\x1b[3~\x1b[15~")},
		{"alt keys pass", [][]byte{[]byte("\x1bc\x1bx")}, []byte("\x1bc\x1bx")},
		{"keypad SS3 passes", [][]byte{[]byte("\x1bOR")}, []byte("\x1bOR")},
		{"mixed garbage and keys", [][]byte{[]byte("\x1b[8;1R\x1b[A\x1b[2;1Rk")}, []byte("\x1b[Ak")},
	}
	for _, c := range cases {
		f := &inputFilter{}
		var got []byte
		for _, ch := range c.in {
			got = append(got, f.feed(ch)...)
		}
		if !bytes.Equal(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// A lone escape waits for the next chunk, then flushes whole.
	f := &inputFilter{}
	if out := f.feed([]byte("x\x1b")); !bytes.Equal(out, []byte("x")) {
		t.Errorf("lone escape: got %q, want %q", out, "x")
	}
	if !f.holding() {
		t.Error("the escape should be held")
	}
	if out := f.flush(); !bytes.Equal(out, []byte("\x1b")) {
		t.Errorf("flush: got %q, want the escape", out)
	}
	// Text after a held escape is classified with it: alt-x passes, a
	// sequence's final byte decides.
	f = &inputFilter{}
	f.feed([]byte("\x1b"))
	if out := f.feed([]byte("x")); !bytes.Equal(out, []byte("\x1bx")) {
		t.Errorf("alt-x: got %q, want %q", out, "\x1bx")
	}
	f = &inputFilter{}
	f.feed([]byte("\x1b["))
	if out := f.feed([]byte("8;59R")); len(out) != 0 {
		t.Errorf("a split CPR should be dropped whole, got %q", out)
	}
}
