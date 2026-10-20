package cot

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protowire"
)

// Magic is the TAK protocol streaming frame marker.
const Magic = 0xbf

// MaxMessageSize bounds a single CoT message on the wire.
const MaxMessageSize = 8 << 20

// Format identifies the wire encoding of a message.
type Format int

const (
	FormatXML Format = iota
	FormatProto
)

func (f Format) String() string {
	if f == FormatProto {
		return "protobuf"
	}
	return "xml"
}

// ErrTooLarge is returned when a message exceeds MaxMessageSize.
var ErrTooLarge = errors.New("cot: message too large")

// MalformedError wraps a message that could not be parsed. The stream is
// still usable after one of these.
type MalformedError struct{ Err error }

func (e *MalformedError) Error() string { return "cot: malformed message: " + e.Err.Error() }
func (e *MalformedError) Unwrap() error { return e.Err }

var endTag = []byte("</event>")

// Reader reads a stream of CoT messages, auto-detecting XML and TAK protobuf
// (0xbf <varint length> <TakMessage>) framing on a per-message basis, so it
// handles the mid-stream switch after protocol negotiation.
type Reader struct {
	r   *bufio.Reader
	buf []byte
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns the next event. Control-only protobuf messages are skipped.
func (r *Reader) Next() (*Event, Format, error) {
	for {
		b, err := r.skipSpace()
		if err != nil {
			return nil, 0, err
		}
		if b == Magic {
			ev, err := r.readProto()
			if err != nil || ev != nil {
				return ev, FormatProto, err
			}
			continue
		}
		ev, err := r.readXML()
		return ev, FormatXML, err
	}
}

func (r *Reader) skipSpace() (byte, error) {
	for {
		b, err := r.r.ReadByte()
		if err != nil {
			return 0, err
		}
		switch b {
		case ' ', '\t', '\r', '\n', 0:
			continue
		}
		return b, r.r.UnreadByte()
	}
}

func (r *Reader) readProto() (*Event, error) {
	if _, err := r.r.ReadByte(); err != nil {
		return nil, err
	}
	n, err := binary.ReadUvarint(r.r)
	if err != nil {
		return nil, err
	}
	if n > MaxMessageSize {
		return nil, ErrTooLarge
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r.r, msg); err != nil {
		return nil, err
	}
	ev, _, err := DecodeTakMessage(msg)
	if err != nil {
		return nil, &MalformedError{err}
	}
	return ev, nil
}

func (r *Reader) readXML() (*Event, error) {
	r.buf = r.buf[:0]
	for {
		chunk, err := r.r.ReadSlice('>')
		r.buf = append(r.buf, chunk...)
		if len(r.buf) > MaxMessageSize {
			return nil, ErrTooLarge
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, err
		}
		if bytes.HasSuffix(r.buf, endTag) {
			break
		}
	}
	i := bytes.Index(r.buf, []byte("<event"))
	if i < 0 {
		return nil, &MalformedError{ErrNotEvent}
	}
	ev, err := ParseXML(bytes.Clone(r.buf[i:]))
	if err != nil {
		return nil, &MalformedError{err}
	}
	return ev, nil
}

// AppendFrame appends ev to b in the given wire format.
func AppendFrame(b []byte, ev *Event, f Format) []byte {
	if f == FormatProto {
		return AppendProtoFrame(b, ev.TakMessage())
	}
	return append(b, ev.XML()...)
}

// AppendProtoFrame frames an encoded TakMessage for streaming.
func AppendProtoFrame(b, msg []byte) []byte {
	b = append(b, Magic)
	b = protowire.AppendVarint(b, uint64(len(msg)))
	return append(b, msg...)
}

// WriteFrame writes ev to w in the given wire format.
func WriteFrame(w io.Writer, ev *Event, f Format) error {
	_, err := w.Write(AppendFrame(nil, ev, f))
	if err != nil {
		return fmt.Errorf("cot: write: %w", err)
	}
	return nil
}
