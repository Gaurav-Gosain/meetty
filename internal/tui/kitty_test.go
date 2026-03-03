package tui

import (
	"strings"
	"testing"
)

func TestKittyBuilder_MoveCursor(t *testing.T) {
	var kb KittyBuilder
	kb.MoveCursor(5, 10) // 0-indexed, becomes 11;6H
	got := string(kb.Bytes())
	want := "\x1b[11;6H"
	if got != want {
		t.Errorf("MoveCursor(5,10) = %q, want %q", got, want)
	}
}

func TestKittyBuilder_MoveCursor_Origin(t *testing.T) {
	var kb KittyBuilder
	kb.MoveCursor(0, 0)
	got := string(kb.Bytes())
	want := "\x1b[1;1H"
	if got != want {
		t.Errorf("MoveCursor(0,0) = %q, want %q", got, want)
	}
}

func TestKittyBuilder_DeleteAll(t *testing.T) {
	var kb KittyBuilder
	kb.DeleteAll()
	got := string(kb.Bytes())
	if !strings.Contains(got, "a=d,d=a") {
		t.Errorf("DeleteAll should contain 'a=d,d=a', got %q", got)
	}
	if !strings.HasPrefix(got, "\x1b_G") {
		t.Errorf("should start with APC, got %q", got[:min(len(got), 10)])
	}
	if !strings.HasSuffix(got, "\x1b\\") {
		t.Errorf("should end with ST, got %q", got)
	}
}

func TestKittyBuilder_DeleteByID(t *testing.T) {
	var kb KittyBuilder
	kb.DeleteByID(42)
	got := string(kb.Bytes())
	if !strings.Contains(got, "d=i,i=42") {
		t.Errorf("DeleteByID(42) should contain 'd=i,i=42', got %q", got)
	}
}

func TestKittyBuilder_TransmitAndPlace_Empty(t *testing.T) {
	var kb KittyBuilder
	kb.TransmitAndPlace(1, nil, 10, 5)
	if kb.buf.Len() != 0 {
		t.Error("TransmitAndPlace with nil data should produce no output")
	}
	kb.TransmitAndPlace(1, []byte{}, 10, 5)
	if kb.buf.Len() != 0 {
		t.Error("TransmitAndPlace with empty data should produce no output")
	}
}

func TestKittyBuilder_TransmitAndPlace_Small(t *testing.T) {
	var kb KittyBuilder
	data := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	kb.TransmitAndPlace(5, data, 20, 10)
	got := string(kb.Bytes())

	if !strings.Contains(got, "a=T") {
		t.Error("missing a=T action")
	}
	if !strings.Contains(got, "f=100") {
		t.Error("missing f=100 (PNG format)")
	}
	if !strings.Contains(got, "i=5") {
		t.Error("missing i=5 (image ID)")
	}
	if !strings.Contains(got, "c=20") {
		t.Error("missing c=20 (columns)")
	}
	if !strings.Contains(got, "r=10") {
		t.Error("missing r=10 (rows)")
	}
}

func TestKittyBuilder_TransmitRGB(t *testing.T) {
	var kb KittyBuilder
	rgb := make([]byte, 100*100*3) // 100x100 RGB image
	kb.TransmitRGB(7, rgb, 100, 100, 30, 15)
	got := string(kb.Bytes())

	if !strings.Contains(got, "a=T") {
		t.Error("missing a=T action")
	}
	if !strings.Contains(got, "f=24") {
		t.Error("missing f=24 (RGB format)")
	}
	if !strings.Contains(got, "o=z") {
		t.Error("missing o=z (zlib compression)")
	}
	if !strings.Contains(got, "s=100") {
		t.Error("missing s=100 (pixel width)")
	}
	if !strings.Contains(got, "v=100") {
		t.Error("missing v=100 (pixel height)")
	}
	if !strings.Contains(got, "i=7") {
		t.Error("missing i=7 (image ID)")
	}
}

func TestKittyBuilder_TransmitRGB_Empty(t *testing.T) {
	var kb KittyBuilder
	kb.TransmitRGB(1, nil, 10, 10, 5, 5)
	if kb.buf.Len() != 0 {
		t.Error("TransmitRGB with nil data should produce no output")
	}
}

func TestKittyBuilder_Chunking(t *testing.T) {
	var kb KittyBuilder
	// Create data large enough to require chunking (>32768 base64 chars)
	large := make([]byte, 32000) // ~42666 base64 chars
	kb.TransmitAndPlace(1, large, 10, 5)
	got := string(kb.Bytes())

	// Should have multiple APC sequences
	count := strings.Count(got, "\x1b_G")
	if count < 2 {
		t.Errorf("expected multiple chunks, got %d APC sequences", count)
	}

	// First chunk should have m=1 (more data coming)
	if !strings.Contains(got, "m=1") {
		t.Error("first chunk should have m=1")
	}

	// Last chunk should have m=0 (no more data)
	if !strings.Contains(got, "m=0") {
		t.Error("last chunk should have m=0")
	}
}

func TestKittyBuilder_Reset(t *testing.T) {
	var kb KittyBuilder
	kb.MoveCursor(0, 0)
	if kb.buf.Len() == 0 {
		t.Error("buffer should have data after MoveCursor")
	}
	kb.Reset()
	if kb.buf.Len() != 0 {
		t.Error("buffer should be empty after Reset")
	}
}

func TestZlibCompress(t *testing.T) {
	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte(i % 256)
	}
	compressed := zlibCompress(data)
	if len(compressed) == 0 {
		t.Error("compressed data should not be empty")
	}
	// zlib should produce smaller output for this data
	if len(compressed) >= len(data) {
		t.Logf("warning: compressed (%d) >= original (%d)", len(compressed), len(data))
	}
}

func BenchmarkTransmitRGB(b *testing.B) {
	rgb := make([]byte, 320*240*3)
	var kb KittyBuilder
	for b.Loop() {
		kb.Reset()
		kb.TransmitRGB(1, rgb, 320, 240, 40, 20)
	}
}
