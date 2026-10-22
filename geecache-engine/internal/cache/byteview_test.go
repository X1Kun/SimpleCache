package cache

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

type shortWriter struct{ err error }

func (w shortWriter) Write(p []byte) (int, error) { return len(p) / 2, w.err }

func TestByteViewWriteTo(t *testing.T) {
	v := ByteView{b: []byte("immutable value")}
	var out bytes.Buffer
	if n, err := v.WriteTo(&out); err != nil || n != int64(v.Len()) || out.String() != v.String() {
		t.Fatalf("write: n=%d err=%v data=%q", n, err, out.String())
	}
	if _, err := v.WriteTo(shortWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	sentinel := errors.New("writer failed")
	if _, err := v.WriteTo(shortWriter{sentinel}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	copy := v.ByteSlice()
	copy[0] = 'X'
	if v.String() != "immutable value" {
		t.Fatal("defensive copy contract changed")
	}
}

type countingWriter struct{ total int }

//go:noinline
func (w *countingWriter) Write(p []byte) (int, error) {
	w.total += len(p)
	if len(p) > 0 {
		w.total += int(p[0])
	}
	return len(p), nil
}

//go:noinline
func writeCopy(w io.Writer, value ByteView) { _, _ = w.Write(value.ByteSlice()) }

func BenchmarkByteViewResponse(b *testing.B) {
	for _, size := range []int{128, 4096, 65536} {
		value := ByteView{b: bytes.Repeat([]byte("v"), size)}
		b.Run(fmt.Sprintf("copy/%d", size), func(b *testing.B) {
			w := &countingWriter{}
			b.ReportAllocs()
			for b.Loop() {
				writeCopy(w, value)
			}
		})
		b.Run(fmt.Sprintf("write-to/%d", size), func(b *testing.B) {
			w := &countingWriter{}
			b.ReportAllocs()
			for b.Loop() {
				_, _ = value.WriteTo(w)
			}
		})
	}
}
