package cookies

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrame_RoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"empty", []byte{}},
		{"tiny", []byte("hello")},
		{"json", []byte(`{"kind":"getCookies","query":{"domain":"claude.ai"}}`)},
		{"kib", bytes.Repeat([]byte{'a'}, 1024)},
		{"256kib", bytes.Repeat([]byte{'b'}, 256*1024)},
		{"at-limit", bytes.Repeat([]byte{'c'}, MaxFrameSize)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteFrame(&buf, tc.payload); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			got, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if !bytes.Equal(got, tc.payload) {
				t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(tc.payload))
			}
		})
	}
}

func TestFrame_MultipleFrames(t *testing.T) {
	var buf bytes.Buffer
	payloads := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	for _, p := range payloads {
		if err := WriteFrame(&buf, p); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	for i, want := range payloads {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d: got %q, want %q", i, got, want)
		}
	}
}

func TestReadFrame_EOFBetweenFrames(t *testing.T) {
	_, err := ReadFrame(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF on empty reader, got %v", err)
	}
}

func TestReadFrame_ShortHeader(t *testing.T) {
	_, err := ReadFrame(bytes.NewReader([]byte{0x01, 0x02}))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestReadFrame_TruncatedBody(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], 10)
	buf.Write(hdr[:])
	buf.Write([]byte("short"))
	_, err := ReadFrame(&buf)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestReadFrame_OverLimit(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], MaxFrameSize+1)
	buf.Write(hdr[:])
	_, err := ReadFrame(&buf)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
}

func TestWriteFrame_OverLimit(t *testing.T) {
	var buf bytes.Buffer
	err := WriteFrame(&buf, bytes.Repeat([]byte{'x'}, MaxFrameSize+1))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
}

type errWriter struct{ err error }

func (e errWriter) Write(p []byte) (int, error) { return 0, e.err }

func TestWriteFrame_PropagatesWriterError(t *testing.T) {
	sentinel := errors.New("write boom")
	err := WriteFrame(errWriter{err: sentinel}, []byte("hi"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
}

func TestWriteFrame_ZeroLenHeaderOnly(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, nil); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if got := buf.Len(); got != 4 {
		t.Fatalf("zero-len frame: want 4 bytes on wire, got %d", got)
	}
	if !bytes.Equal(buf.Bytes(), []byte{0, 0, 0, 0}) {
		t.Fatalf("zero-len header bytes: got % x", buf.Bytes())
	}
}

// Guard against an accidental binary.BigEndian slip — Chrome's spec is
// strictly little-endian.
func TestWriteFrame_LittleEndianHeader(t *testing.T) {
	var buf bytes.Buffer
	payload := strings.Repeat("a", 258) // 0x102 — distinct across endiannesses
	if err := WriteFrame(&buf, []byte(payload)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got := buf.Bytes()[:4]
	want := []byte{0x02, 0x01, 0x00, 0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("header: got % x, want % x", got, want)
	}
}
