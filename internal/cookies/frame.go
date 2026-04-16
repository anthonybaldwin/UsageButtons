package cookies

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxFrameSize limits a single native-messaging frame payload. Chrome
// caps host→extension messages at 1 MiB; we apply the same bound in
// both directions, which is ample for cookie bundles and prevents a
// malformed header from triggering a runaway allocation.
const MaxFrameSize = 1 << 20 // 1 MiB

// ErrFrameTooLarge is returned when a frame's declared length exceeds
// MaxFrameSize.
var ErrFrameTooLarge = errors.New("cookies: native-messaging frame exceeds size limit")

// ReadFrame reads one Chrome native-messaging frame from r: a 4-byte
// little-endian length prefix followed by that many bytes of UTF-8
// JSON payload. Returns io.EOF cleanly if the reader is closed between
// frames, and io.ErrUnexpectedEOF if the header or body is truncated.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n == 0 {
		return []byte{}, nil
	}
	if n > MaxFrameSize {
		return nil, fmt.Errorf("%w: got %d bytes, max %d", ErrFrameTooLarge, n, MaxFrameSize)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// WriteFrame writes one native-messaging frame to w.
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("%w: got %d bytes, max %d", ErrFrameTooLarge, len(payload), MaxFrameSize)
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}
