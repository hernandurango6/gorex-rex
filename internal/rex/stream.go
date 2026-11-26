package rex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Stream is a terminal's data connection to a block, as a terminal's
// Conn: Read returns the snapshot's restoring sequences then the
// program's output, Write types input, and Resize changes its size.
type Stream struct {
	c     *Client
	sid   string
	block string

	mu       sync.Mutex
	cond     *sync.Cond
	chunks   [][]byte
	cols     int
	rows     int
	sentCols int
	sentRows int
	err      error
	nonce    uint64
	apcs     bytes.Buffer // a Rex message the stream split
	closed   bool
	ws       *websocket.Conn

	sizeTimer *time.Timer
	writes    chan []byte
	lastWrite atomic.Int64 // Unix nanos of the last input sent

	// OnData, when set, is called with the size of each read.
	OnData func(n int)
	// OnResize is called when the server reports the terminal's size,
	// such as when another client resizes it.
	OnResize func(cols, rows int)
	// OnAPC is called for the stream's other messages: child_exited
	// among them.
	OnAPC func(kind string, payload json.RawMessage)
}

// Stream attaches to the block's terminal data source in the
// background: the first reads yield the snapshot as restoring
// sequences, then what the program prints.
func (c *Client) Stream(sid, block string, cols, rows int) *Stream {
	s := &Stream{c: c, sid: sid, block: block, cols: cols, rows: rows,
		writes: make(chan []byte, 512)}
	s.cond = sync.NewCond(&s.mu)
	go s.run()
	go s.writer()
	return s
}

// fatalError is a connect error no retry can fix, such as a missing
// decoder library.
type fatalError struct{ err error }

func (e fatalError) Error() string { return e.err.Error() }
func (e fatalError) Unwrap() error { return e.err }

// run opens the data connection, feeds Read, and reopens it while the
// stream is worth resuming.
func (s *Stream) run() {
	var err error
	for range 30 {
		err = s.connect()
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		var fe fatalError
		if err == nil || err == io.EOF || errors.As(err, &fe) || closed {
			break
		}
		time.Sleep(700 * time.Millisecond)
	}
	var fe fatalError
	if errors.As(err, &fe) {
		err = fe.err
	}
	if err != nil && err != io.EOF {
		log.Printf("rex: stream %s ended: %v", s.block, err)
	}
	s.mu.Lock()
	s.err = err
	s.cond.Broadcast()
	s.mu.Unlock()
	s.closeWrites()
}

// connect runs the handshake and relays the stream until it ends.
func (s *Stream) connect() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t, err := s.c.DataConnect(ctx, s.sid, s.block)
	cancel()
	if err != nil {
		return err
	}
	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	ws, err := dialWS(ctx, s.c.Ep, "/api/data", "rex.data.terminal.v1")
	cancel()
	if err != nil {
		return err
	}
	// The opening snapshot is one message of the terminal's whole
	// scrollback; the read limit the control connection sets applies here
	// too, since the default's 32 KiB ends big snapshots mid-decode.
	ws.SetReadLimit(16 << 20)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ws.Close(websocket.StatusNormalClosure, "")
		return io.EOF
	}
	s.ws = ws
	s.mu.Unlock()
	defer ws.Close(websocket.StatusNormalClosure, "")
	tb, err := base64.RawURLEncoding.DecodeString(t.Ticket)
	if err != nil || len(tb) != 16 {
		return errors.New("bad data ticket")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	err = ws.Write(ctx, websocket.MessageBinary, append([]byte{0x01}, tb...))
	cancel()
	if err != nil {
		return err
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	typ, w, err := ws.Read(ctx)
	cancel()
	if err != nil {
		return err
	}
	if typ != websocket.MessageBinary || len(w) < 2 {
		return fmt.Errorf("bad data handshake: %d bytes", len(w))
	}
	switch w[0] {
	case 0x81:
		if len(w) != 32 {
			return fmt.Errorf("bad WELCOME of %d bytes", len(w))
		}
		s.mu.Lock()
		s.nonce = binary.LittleEndian.Uint64(w[24:32])
		s.mu.Unlock()
	case 0x82:
		return fmt.Errorf("data handshake rejected: reason %#x", w[1])
	default:
		return fmt.Errorf("bad handshake byte %#x", w[0])
	}

	// The stream opens with a snapshot of the terminal: decode it into
	// the sequences that restore it, then relay the rest raw.
	feed := &frameFeed{ws: ws}
	if snapshotSupported {
		path := s.c.GhosttyPath
		if path == "" {
			path = os.Getenv("MYGO_GHOSTTY_VT")
		}
		if path == "" {
			return fatalError{errors.New("rex: no libghostty-vt to decode the snapshot with")}
		}
		if err := ghosttyLoad(path); err != nil {
			return fatalError{fmt.Errorf("rex: loading libghostty-vt: %w", err)}
		}
		vt, rest, decErr := snapshotVT(func(p []byte) int {
			return feed.pull(p)
		})
		if decErr != nil {
			return decErr
		}
		// The decoder may have read past the snapshot's end: its leftovers
		// and the feed's are the stream's first output.
		if len(rest) > 0 {
			feed.buf = append(rest, feed.buf...)
		}
		if len(feed.buf) > 0 {
			if !s.relay(feed.buf) {
				return nil
			}
			feed.buf = nil
		}
		if len(vt) > 0 && !s.push(vt) {
			return nil
		}
	}
	// Size the terminal once attached: as its owner, the app's sizes
	// apply.
	s.mu.Lock()
	cols, rows := s.cols, s.rows
	s.mu.Unlock()
	if cols > 0 && rows > 0 {
		go s.c.ResizeTerm(context.Background(), s.sid, s.block, cols, rows)
	}
	for {
		typ, data, err := ws.Read(context.Background())
		if err != nil {
			return s.closeErr(err)
		}
		if typ != websocket.MessageBinary || len(data) == 0 {
			continue
		}
		if !s.relay(data) {
			return nil
		}
	}
}

// frameFeed reads the stream for the snapshot decoder: whole frames of
// the data connection, whose unread part buf holds.
type frameFeed struct {
	ws  *websocket.Conn
	buf []byte
	err error
}

// pull copies the feed's buffered bytes into p for the decoder; 0 at
// the stream's end or on error.
func (f *frameFeed) pull(p []byte) int {
	for len(f.buf) == 0 && f.err == nil {
		var typ websocket.MessageType
		var data []byte
		typ, data, f.err = f.ws.Read(context.Background())
		if typ != websocket.MessageBinary && f.err == nil {
			continue
		}
		f.buf = data
	}
	if len(f.buf) == 0 {
		if f.err != nil {
			return -1
		}
		return 0
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n
}

// relay takes one frame of the stream: Rex's own messages come out of
// it, and the rest is output for the terminal.
func (s *Stream) relay(data []byte) bool {
	for len(data) > 0 {
		if s.apcs.Len() > 0 {
			i := bytes.Index(data, []byte("\x1b\\"))
			if i < 0 {
				s.apcs.Write(data)
				return true
			}
			s.apcs.Write(data[:i+2])
			m := append([]byte(nil), s.apcs.Bytes()...)
			s.apcs.Reset()
			if !s.handleAPC(m) {
				return false
			}
			data = data[i+2:]
			continue
		}
		i := bytes.Index(data, []byte("\x1b_rex1;"))
		if i < 0 {
			return s.push(data)
		}
		if !s.push(data[:i]) {
			return false
		}
		data = data[i:]
		j := bytes.Index(data, []byte("\x1b\\"))
		if j < 0 {
			s.apcs.Write(data)
			return true
		}
		if !s.handleAPC(data[:j+2]) {
			return false
		}
		data = data[j+2:]
	}
	return true
}

// handleAPC reads one Rex message of the stream, ESC _ rex1;<nonce>;
// <json> ESC \. One with another nonce is text the program printed,
// which goes through as output.
func (s *Stream) handleAPC(m []byte) bool {
	body := m[len("\x1b_rex1;") : len(m)-2]
	i := bytes.IndexByte(body, ';')
	if i < 0 {
		return s.push(m)
	}
	n, err := strconv.ParseUint(string(body[:i]), 10, 64)
	if err != nil || n != s.nonce {
		return s.push(m)
	}
	raw := append(json.RawMessage(nil), body[i+1:]...)
	var msg struct {
		Type string `json:"type"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	}
	json.Unmarshal(raw, &msg)
	if msg.Type == "resize" {
		s.mu.Lock()
		s.sentCols, s.sentRows = msg.Cols, msg.Rows
		s.mu.Unlock()
		if s.OnResize != nil {
			s.OnResize(msg.Cols, msg.Rows)
		}
	}
	if s.OnAPC != nil {
		s.OnAPC(msg.Type, raw)
	}
	return true
}

// push gives output to Read; false means the stream is closed.
func (s *Stream) push(p []byte) bool {
	if len(p) == 0 {
		return true
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.chunks = append(s.chunks, p)
	s.cond.Signal()
	s.mu.Unlock()
	return true
}

// closeErr reports the error Read returns after the last data.
func (s *Stream) closeErr(err error) error {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		if ce.Code == websocket.StatusNormalClosure || ce.Code == 4000 {
			return io.EOF
		}
		return fmt.Errorf("data connection closed: %s (%d)", ce.Reason, ce.Code)
	}
	return io.EOF
}

// Read returns the terminal's output: the snapshot's restoring
// sequences first, then what the program prints, with Rex's own
// messages taken out.
func (s *Stream) Read(p []byte) (int, error) {
	s.mu.Lock()
	for len(s.chunks) == 0 && s.err == nil && !s.closed {
		s.cond.Wait()
	}
	if len(s.chunks) == 0 {
		err := s.err
		s.mu.Unlock()
		if s.closed || err == nil {
			return 0, io.EOF
		}
		return 0, err
	}
	b := s.chunks[0]
	n := copy(p, b)
	if n == len(b) {
		s.chunks = s.chunks[1:]
	} else {
		s.chunks[0] = b[n:]
	}
	s.mu.Unlock()
	if s.OnData != nil {
		s.OnData(n)
	}
	return n, nil
}

// Write types input into the terminal; a goroutine sends each chunk in
// order as terminal.write calls.
func (s *Stream) Write(p []byte) (int, error) {
	b := append([]byte(nil), p...)
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return 0, io.EOF
	}
	select {
	case s.writes <- b:
		return len(p), nil
	default:
		// A stream far behind drops input rather than grow without end.
		return len(p), nil
	}
}

func (s *Stream) closeWrites() {
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
	close(s.writes)
}

// writer sends each chunk of input in order, as terminal.write calls. A
// pipe endpoint's server is a Windows one, whose ConPTY shells read
// input records: the filter takes the emulator's answers to the
// program's queries out of their input, which would type them as
// keystrokes.
func (s *Stream) writer() {
	var filter *inputFilter
	if s.c.Ep.Pipe != "" {
		filter = &inputFilter{}
	}
	send := func(b []byte) {
		if len(b) == 0 {
			return
		}
		s.lastWrite.Store(time.Now().UnixNano())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := s.c.Write(ctx, s.sid, s.block, b); err != nil {
			log.Printf("rex: write %s: %v", s.block, err)
		}
		cancel()
	}
	for {
		// A chunk ending in a partial sequence waits briefly for the
		// rest; a lone escape, as vim's, flushes after the pause PSReadLine
		// itself allows for one.
		var timeout <-chan time.Time
		var timer *time.Timer
		if filter != nil && filter.holding() {
			timer = time.NewTimer(60 * time.Millisecond)
			timeout = timer.C
		}
		var b []byte
		var ok bool
		select {
		case b, ok = <-s.writes:
			if timer != nil {
				timer.Stop()
			}
			if !ok {
				if filter != nil {
					send(filter.flush())
				}
				return
			}
		case <-timeout:
			send(filter.flush())
			continue
		}
		if filter != nil {
			b = filter.feed(b)
		}
		send(b)
	}
}

// Err is the error the stream ended with, nil while it runs.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// LastWrite is when the stream last sent input, as Unix nanoseconds; 0
// if never.
func (s *Stream) LastWrite() int64 { return s.lastWrite.Load() }

// Resize asks the block's terminal for the size, debounced so a drag
// sends its last one.
func (s *Stream) Resize(cols, rows int) error {
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	if s.sizeTimer == nil {
		s.sizeTimer = time.AfterFunc(150*time.Millisecond, s.sendSize)
	} else {
		s.sizeTimer.Reset(150 * time.Millisecond)
	}
	s.mu.Unlock()
	return nil
}

func (s *Stream) sendSize() {
	s.mu.Lock()
	c, r := s.cols, s.rows
	closed := s.closed
	if c > 0 && r > 0 && (c != s.sentCols || r != s.sentRows) {
		s.sentCols, s.sentRows = c, r
	} else {
		c = 0
	}
	s.mu.Unlock()
	if c > 0 && !closed {
		s.c.ResizeTerm(context.Background(), s.sid, s.block, c, r)
	}
}

// Close ends the data connection; the block's program goes on.
func (s *Stream) Close() error {
	s.mu.Lock()
	s.closed = true
	ws := s.ws
	s.cond.Broadcast()
	s.mu.Unlock()
	if ws != nil {
		ws.Close(websocket.StatusNormalClosure, "")
	}
	return nil
}

// Size returns the size the stream was last given.
func (s *Stream) Size() (cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}
