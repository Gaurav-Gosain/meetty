//go:build linux

package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// CameraDevice represents an available webcam.
type CameraDevice struct {
	Path string
	Name string
}

// V4L2 constants
const (
	v4l2CapVideoCapture     uint32 = 0x00000001
	v4l2CapStreaming        uint32 = 0x04000000
	v4l2BufTypeVideoCapture uint32 = 1
	v4l2MemoryMmap          uint32 = 1
	v4l2FieldAny            uint32 = 0

	v4l2FrmsizeTypeDiscrete uint32 = 1

	v4l2FormatMJPEG uint32 = 0x47504A4D // 'MJPG'

	numBuffers = 4
)

// ioctl number helpers (Linux ioctl encoding)
const (
	iocWrite = 1
	iocRead  = 2

	iocNrBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14

	iocNrShift   = 0
	iocTypeShift = iocNrShift + iocNrBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits
)

func ioc(dir, t, nr, size uintptr) uintptr {
	return (dir << iocDirShift) | (t << iocTypeShift) | (nr << iocNrShift) | (size << iocSizeShift)
}

func ior(t, nr, size uintptr) uintptr  { return ioc(iocRead, t, nr, size) }
func iow(t, nr, size uintptr) uintptr  { return ioc(iocWrite, t, nr, size) }
func iorw(t, nr, size uintptr) uintptr { return ioc(iocRead|iocWrite, t, nr, size) }

func ioctl(fd, op, arg uintptr) error {
	_, _, ep := unix.Syscall(unix.SYS_IOCTL, fd, op, arg)
	if ep != 0 {
		return ep
	}
	return nil
}

// V4L2 structs (matching kernel videodev2.h layout)

type v4l2Capability struct {
	Driver       [16]uint8
	Card         [32]uint8
	BusInfo      [32]uint8
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

type v4l2FmtDesc struct {
	Index       uint32
	Type        uint32
	Flags       uint32
	Description [32]uint8
	PixelFormat uint32
	Reserved    [4]uint32
}

type v4l2PixFormat struct {
	Width        uint32
	Height       uint32
	PixelFormat  uint32
	Field        uint32
	BytesPerLine uint32
	SizeImage    uint32
	ColorSpace   uint32
	Priv         uint32
	Flags        uint32
	YcbcrEnc     uint32
	Quantization uint32
	XferFunc     uint32
}

// ptrSize is the size of a pointer on this platform.
const ptrSize = unsafe.Sizeof(uintptr(0))

type v4l2FormatUnion struct {
	Data [200 - ptrSize]byte
	_    uintptr // force pointer alignment
}

type v4l2Format struct {
	Type  uint32
	Union v4l2FormatUnion
}

type v4l2RequestBuffers struct {
	Count    uint32
	Type     uint32
	Memory   uint32
	Reserved [2]uint32
}

type v4l2Timecode struct {
	Type     uint32
	Flags    uint32
	Frames   uint8
	Seconds  uint8
	Minutes  uint8
	Hours    uint8
	UserBits [4]uint8
}

type v4l2Buffer struct {
	Index     uint32
	Type      uint32
	BytesUsed uint32
	Flags     uint32
	Field     uint32
	Timestamp unix.Timeval
	Timecode  v4l2Timecode
	Sequence  uint32
	Memory    uint32
	Union     [ptrSize]uint8
	Length    uint32
	Reserved2 uint32
	Reserved  uint32
}

type v4l2FrmSizeEnum struct {
	Index       uint32
	PixelFormat uint32
	Type        uint32
	Union       [24]uint8
	Reserved    [2]uint32
}

type v4l2FrmSizeDiscrete struct {
	Width  uint32
	Height uint32
}

// ioctl numbers
var (
	vidiocQuerycap       = ior(uintptr('V'), 0, unsafe.Sizeof(v4l2Capability{}))
	vidiocEnumFmt        = iorw(uintptr('V'), 2, unsafe.Sizeof(v4l2FmtDesc{}))
	vidiocSFmt           = iorw(uintptr('V'), 5, unsafe.Sizeof(v4l2Format{}))
	vidiocReqbufs        = iorw(uintptr('V'), 8, unsafe.Sizeof(v4l2RequestBuffers{}))
	vidiocQuerybuf       = iorw(uintptr('V'), 9, unsafe.Sizeof(v4l2Buffer{}))
	vidiocQBuf           = iorw(uintptr('V'), 15, unsafe.Sizeof(v4l2Buffer{}))
	vidiocDQBuf          = iorw(uintptr('V'), 17, unsafe.Sizeof(v4l2Buffer{}))
	vidiocStreamOn       = iow(uintptr('V'), 18, 4)
	vidiocStreamOff      = iow(uintptr('V'), 19, 4)
	vidiocEnumFrameSizes = iorw(uintptr('V'), 74, unsafe.Sizeof(v4l2FrmSizeEnum{}))
)

var nativeByteOrder = getNativeByteOrder()

func getNativeByteOrder() binary.ByteOrder {
	var i int32 = 0x01020304
	b := *(*byte)(unsafe.Pointer(&i))
	if b == 0x04 {
		return binary.LittleEndian
	}
	return binary.BigEndian
}

// ListCameras returns video capture devices that support MJPEG.
func ListCameras() []CameraDevice {
	matches, _ := filepath.Glob("/dev/video*")
	var devices []CameraDevice
	for _, path := range matches {
		handle, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0666)
		if err != nil {
			continue
		}
		fd := uintptr(handle)

		// Check capabilities
		caps := &v4l2Capability{}
		if ioctl(fd, vidiocQuerycap, uintptr(unsafe.Pointer(caps))) != nil {
			unix.Close(handle)
			continue
		}
		if caps.Capabilities&v4l2CapVideoCapture == 0 {
			unix.Close(handle)
			continue
		}

		// Check for MJPEG format
		hasMJPEG := false
		for i := uint32(0); ; i++ {
			fmtdesc := &v4l2FmtDesc{Index: i, Type: v4l2BufTypeVideoCapture}
			if ioctl(fd, vidiocEnumFmt, uintptr(unsafe.Pointer(fmtdesc))) != nil {
				break
			}
			if fmtdesc.PixelFormat == v4l2FormatMJPEG {
				hasMJPEG = true
				break
			}
		}
		unix.Close(handle)

		if !hasMJPEG {
			continue
		}
		devices = append(devices, CameraDevice{Path: path, Name: readDeviceName(path)})
	}
	return devices
}

// readDeviceName reads the device name from sysfs.
func readDeviceName(devPath string) string {
	base := filepath.Base(devPath)
	nameFile := filepath.Join("/sys/class/video4linux", base, "name")
	data, err := os.ReadFile(nameFile)
	if err != nil {
		return base
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return base
	}
	return name
}

// bestFrameSize picks the largest discrete frame size that fits within maxW x maxH.
func bestFrameSize(fd uintptr, pixelFormat uint32, maxW, maxH uint32) (uint32, uint32) {
	var bestW, bestH uint32
	for i := uint32(0); ; i++ {
		fse := &v4l2FrmSizeEnum{Index: i, PixelFormat: pixelFormat}
		if ioctl(fd, vidiocEnumFrameSizes, uintptr(unsafe.Pointer(fse))) != nil {
			break
		}
		if fse.Type != v4l2FrmsizeTypeDiscrete {
			continue
		}
		discrete := &v4l2FrmSizeDiscrete{}
		if err := binary.Read(bytes.NewReader(fse.Union[:]), nativeByteOrder, discrete); err != nil {
			continue
		}
		w, h := discrete.Width, discrete.Height
		if w <= maxW && h <= maxH && w*h > bestW*bestH {
			bestW, bestH = w, h
		}
	}
	if bestW == 0 {
		return maxW, maxH
	}
	return bestW, bestH
}

// CaptureWebcam captures frames from the webcam using V4L2.
func CaptureWebcam(ctx context.Context, device string, fps int, frameCh chan<- []byte) error {
	return captureWebcamV4L2(ctx, device, fps, frameCh)
}

// captureWebcamV4L2 captures frames from the webcam using native V4L2.
// Automatically picks the best resolution up to CameraMaxWidth x CameraMaxHeight.
// Sends raw MJPEG frames (JPEG bytes) on frameCh.
func captureWebcamV4L2(ctx context.Context, device string, fps int, frameCh chan<- []byte) error {
	if device == "" {
		device = "/dev/video0"
	}
	if fps <= 0 {
		fps = CameraFPS
	}
	_ = fps // used for future frame rate control via VIDIOC_S_PARM

	handle, err := unix.Open(device, unix.O_RDWR|unix.O_NONBLOCK, 0666)
	if err != nil {
		return fmt.Errorf("open webcam %s: %w", device, err)
	}
	fd := uintptr(handle)
	defer unix.Close(handle)

	// Check capabilities
	caps := &v4l2Capability{}
	if err := ioctl(fd, vidiocQuerycap, uintptr(unsafe.Pointer(caps))); err != nil {
		return fmt.Errorf("query capabilities: %w", err)
	}
	if caps.Capabilities&v4l2CapVideoCapture == 0 {
		return fmt.Errorf("device does not support video capture")
	}
	if caps.Capabilities&v4l2CapStreaming == 0 {
		return fmt.Errorf("device does not support streaming")
	}

	// Find MJPEG format
	hasMJPEG := false
	for i := uint32(0); ; i++ {
		fmtdesc := &v4l2FmtDesc{Index: i, Type: v4l2BufTypeVideoCapture}
		if ioctl(fd, vidiocEnumFmt, uintptr(unsafe.Pointer(fmtdesc))) != nil {
			break
		}
		if fmtdesc.PixelFormat == v4l2FormatMJPEG {
			hasMJPEG = true
			break
		}
	}
	if !hasMJPEG {
		return fmt.Errorf("device does not support MJPEG format")
	}

	reqW, reqH := bestFrameSize(fd, v4l2FormatMJPEG, CameraMaxWidth, CameraMaxHeight)

	// Set image format
	format := &v4l2Format{Type: v4l2BufTypeVideoCapture}
	pix := v4l2PixFormat{
		Width:       reqW,
		Height:      reqH,
		PixelFormat: v4l2FormatMJPEG,
		Field:       v4l2FieldAny,
	}
	pixBuf := &bytes.Buffer{}
	binary.Write(pixBuf, nativeByteOrder, pix)
	copy(format.Union.Data[:], pixBuf.Bytes())

	if err := ioctl(fd, vidiocSFmt, uintptr(unsafe.Pointer(format))); err != nil {
		return fmt.Errorf("set image format: %w", err)
	}

	// Request mmap buffers
	reqbufs := &v4l2RequestBuffers{
		Count:  numBuffers,
		Type:   v4l2BufTypeVideoCapture,
		Memory: v4l2MemoryMmap,
	}
	if err := ioctl(fd, vidiocReqbufs, uintptr(unsafe.Pointer(reqbufs))); err != nil {
		return fmt.Errorf("request buffers: %w", err)
	}

	// Map buffers
	type mmapBuf struct {
		data []byte
	}
	buffers := make([]mmapBuf, reqbufs.Count)
	for i := uint32(0); i < reqbufs.Count; i++ {
		buf := &v4l2Buffer{
			Type:   v4l2BufTypeVideoCapture,
			Memory: v4l2MemoryMmap,
			Index:  i,
		}
		if err := ioctl(fd, vidiocQuerybuf, uintptr(unsafe.Pointer(buf))); err != nil {
			return fmt.Errorf("query buffer %d: %w", i, err)
		}

		var offset uint32
		if err := binary.Read(bytes.NewReader(buf.Union[:]), nativeByteOrder, &offset); err != nil {
			return fmt.Errorf("read buffer offset: %w", err)
		}

		mapped, err := unix.Mmap(handle, int64(offset), int(buf.Length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("mmap buffer %d: %w", i, err)
		}
		buffers[i] = mmapBuf{data: mapped}
	}

	defer func() {
		for _, b := range buffers {
			unix.Munmap(b.data)
		}
	}()

	// Enqueue all buffers
	for i := uint32(0); i < reqbufs.Count; i++ {
		buf := &v4l2Buffer{
			Type:   v4l2BufTypeVideoCapture,
			Memory: v4l2MemoryMmap,
			Index:  i,
		}
		if err := ioctl(fd, vidiocQBuf, uintptr(unsafe.Pointer(buf))); err != nil {
			return fmt.Errorf("enqueue buffer %d: %w", i, err)
		}
	}

	// Start streaming
	bufType := v4l2BufTypeVideoCapture
	if err := ioctl(fd, vidiocStreamOn, uintptr(unsafe.Pointer(&bufType))); err != nil {
		return fmt.Errorf("start streaming: %w", err)
	}
	defer func() {
		bt := v4l2BufTypeVideoCapture
		ioctl(fd, vidiocStreamOff, uintptr(unsafe.Pointer(&bt)))
	}()

	// Poll for frames
	pollFds := []unix.PollFd{{Fd: int32(handle), Events: unix.POLLIN}}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Poll with 1 second timeout
		n, err := unix.Poll(pollFds, 1000)
		if n < 0 && err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("poll: %w", err)
		}
		if n == 0 {
			continue // timeout, retry
		}

		// Dequeue buffer
		dqbuf := &v4l2Buffer{
			Type:   v4l2BufTypeVideoCapture,
			Memory: v4l2MemoryMmap,
		}
		if err := ioctl(fd, vidiocDQBuf, uintptr(unsafe.Pointer(dqbuf))); err != nil {
			if err == unix.EAGAIN {
				continue
			}
			return fmt.Errorf("dequeue buffer: %w", err)
		}

		// Copy frame data (buffer is reused by kernel)
		frameLen := int(dqbuf.BytesUsed)
		if frameLen > 0 && int(dqbuf.Index) < len(buffers) {
			frameCopy := make([]byte, frameLen)
			copy(frameCopy, buffers[dqbuf.Index].data[:frameLen])

			// Non-blocking send
			select {
			case frameCh <- frameCopy:
			default:
			}
		}

		// Re-enqueue buffer
		qbuf := &v4l2Buffer{
			Type:   v4l2BufTypeVideoCapture,
			Memory: v4l2MemoryMmap,
			Index:  dqbuf.Index,
		}
		if err := ioctl(fd, vidiocQBuf, uintptr(unsafe.Pointer(qbuf))); err != nil {
			return fmt.Errorf("re-enqueue buffer: %w", err)
		}
	}
}
