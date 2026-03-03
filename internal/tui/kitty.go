package tui

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"strconv"
	"sync"
)

const kittyChunkSize = 32768 // larger chunks = fewer escape sequences = faster transmission

// KittyBuilder builds kitty graphics protocol escape sequences.
type KittyBuilder struct {
	buf bytes.Buffer
}

// Reset clears the buffer.
func (b *KittyBuilder) Reset() {
	b.buf.Reset()
}

// Bytes returns the built escape sequences.
func (b *KittyBuilder) Bytes() []byte {
	return b.buf.Bytes()
}

// MoveCursor moves the terminal cursor to the given position (0-indexed).
func (b *KittyBuilder) MoveCursor(x, y int) {
	b.buf.WriteString("\x1b[")
	b.buf.WriteString(strconv.Itoa(y + 1))
	b.buf.WriteByte(';')
	b.buf.WriteString(strconv.Itoa(x + 1))
	b.buf.WriteByte('H')
}

// TransmitAndPlace sends a PNG image and displays it at the current cursor
// position. Uses a=T (transmit+display), f=100 (PNG), q=2 (quiet).
func (b *KittyBuilder) TransmitAndPlace(id uint32, pngData []byte, cols, rows int) {
	if len(pngData) == 0 {
		return
	}
	encoded := base64.StdEncoding.EncodeToString(pngData)
	params := fmt.Sprintf("a=T,f=100,i=%d,c=%d,r=%d,C=1,q=2", id, cols, rows)
	b.writeChunked(params, encoded)
}

// TransmitRGB sends raw 24-bit RGB pixel data with zlib compression.
// Uses a=T,f=24,o=z. Much faster than PNG encoding since we skip the
// PNG filter+compress pipeline.
func (b *KittyBuilder) TransmitRGB(id uint32, rgbData []byte, pixelW, pixelH, cols, rows int) {
	if len(rgbData) == 0 || pixelW <= 0 || pixelH <= 0 || cols <= 0 || rows <= 0 {
		return
	}
	// Validate RGB data matches declared pixel dimensions.
	if len(rgbData) != pixelW*pixelH*3 {
		return
	}
	compressed := zlibCompress(rgbData)
	encoded := base64.StdEncoding.EncodeToString(compressed)
	params := fmt.Sprintf("a=T,f=24,o=z,s=%d,v=%d,i=%d,c=%d,r=%d,C=1,q=2", pixelW, pixelH, id, cols, rows)
	b.writeChunked(params, encoded)
}

// BeginSync starts synchronized output mode (DEC 2026).
// All output between BeginSync and EndSync is buffered and rendered atomically.
func (b *KittyBuilder) BeginSync() {
	b.buf.WriteString("\x1b[?2026h")
}

// EndSync ends synchronized output mode.
func (b *KittyBuilder) EndSync() {
	b.buf.WriteString("\x1b[?2026l")
}

// DeleteAll deletes all kitty images from the terminal.
func (b *KittyBuilder) DeleteAll() {
	b.writeAPC("a=d,d=a,q=2", "")
}

// DeleteByID deletes a specific image by ID.
func (b *KittyBuilder) DeleteByID(id uint32) {
	b.writeAPC(fmt.Sprintf("a=d,d=i,i=%d,q=2", id), "")
}

func (b *KittyBuilder) writeChunked(params, encoded string) {
	if len(encoded) <= kittyChunkSize {
		b.writeAPC(params, encoded)
		return
	}

	for offset := 0; offset < len(encoded); offset += kittyChunkSize {
		end := min(offset+kittyChunkSize, len(encoded))
		chunk := encoded[offset:end]
		more := end < len(encoded)

		if offset == 0 {
			p := params
			if more {
				p += ",m=1"
			}
			b.writeAPC(p, chunk)
		} else {
			if more {
				b.writeAPC("m=1,q=2", chunk)
			} else {
				b.writeAPC("m=0,q=2", chunk)
			}
		}
	}
}

func (b *KittyBuilder) writeAPC(params, data string) {
	b.buf.WriteString("\x1b_G")
	b.buf.WriteString(params)
	if data != "" {
		b.buf.WriteByte(';')
		b.buf.WriteString(data)
	}
	b.buf.WriteString("\x1b\\")
}

// zlibWriterPool reuses zlib writers to avoid repeated initialization.
var zlibWriterPool = sync.Pool{
	New: func() any {
		w, _ := zlib.NewWriterLevel(nil, zlib.BestSpeed)
		return w
	},
}

// zlibBufPool reuses output buffers for zlib compression.
var zlibBufPool sync.Pool

// zlibCompress compresses data with zlib BestSpeed using pooled writers.
func zlibCompress(data []byte) []byte {
	var buf *bytes.Buffer
	if pooled, ok := zlibBufPool.Get().(*bytes.Buffer); ok {
		pooled.Reset()
		buf = pooled
	} else {
		buf = &bytes.Buffer{}
	}
	buf.Grow(len(data) / 2)

	w := zlibWriterPool.Get().(*zlib.Writer)
	w.Reset(buf)
	if _, err := w.Write(data); err != nil {
		zlibWriterPool.Put(w)
		zlibBufPool.Put(buf)
		return data
	}
	if err := w.Close(); err != nil {
		zlibWriterPool.Put(w)
		zlibBufPool.Put(buf)
		return data
	}
	zlibWriterPool.Put(w)

	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	zlibBufPool.Put(buf)
	return result
}
