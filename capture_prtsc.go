package main

import (
	"encoding/binary"
	"fmt"
	"image"
	"log"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PrintScreen capture puts a full-screen bitmap on the clipboard (via Space
// injecting PrtSc, or a physical PrtSc key), then crops the selected region.

const (
	CF_DIB                = 8
	CF_DIBV5              = 17
	CF_UNICODETEXT        = 13
	BI_RGB                = 0
	BI_BITFIELDS          = 3
	GMEM_MOVEABLE         = 0x0002
	VK_SNAPSHOT           = 0x2C
	KEYEVENTF_EXTENDEDKEY = 0x0001
	KEYEVENTF_KEYUP       = 0x0002
	printScreenTimeout    = 5 * time.Second
	maxClipboardText      = 256 * 1024
)

var (
	kernel32 = windows.NewLazyDLL("kernel32.dll")
	dwmapi   = windows.NewLazyDLL("dwmapi.dll")

	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	procKeybdEvent                 = user32.NewProc("keybd_event")
	procGlobalAlloc                = kernel32.NewProc("GlobalAlloc")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procGlobalSize                 = kernel32.NewProc("GlobalSize")
	procGlobalFree                 = kernel32.NewProc("GlobalFree")
	procDwmFlush                   = dwmapi.NewProc("DwmFlush")
)

type clipboardFormat struct {
	id   uint32
	data []byte
}

func sendPrintScreen() {
	// Extended PrintScreen scan code (0xE037). Injected events are ignored by
	// our own PrtSc hook path via LLKHF_INJECTED.
	procKeybdEvent.Call(VK_SNAPSHOT, 0x37, KEYEVENTF_EXTENDEDKEY, 0)
	time.Sleep(40 * time.Millisecond)
	procKeybdEvent.Call(VK_SNAPSHOT, 0x37, KEYEVENTF_EXTENDEDKEY|KEYEVENTF_KEYUP, 0)
}

func snapshotUnicodeText() ([]byte, bool) {
	if !tryOpenClipboard() {
		return nil, false
	}
	defer procCloseClipboard.Call()

	handle, _, _ := procGetClipboardData.Call(CF_UNICODETEXT)
	if handle == 0 {
		return nil, false
	}
	size, _, _ := procGlobalSize.Call(handle)
	if size == 0 || size > maxClipboardText {
		return nil, false
	}
	data, err := copyGlobal(handle)
	if err != nil {
		return nil, false
	}
	return data, true
}

func waitForNewClipboardDIB(prevSeq uint32, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		seq, _, _ := procGetClipboardSequenceNumber.Call()
		if uint32(seq) != prevSeq {
			data, err := readClipboardDIB()
			if err == nil {
				return data, nil
			}
			lastErr = err
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("PrintScreen changed the clipboard but no bitmap was found: %v", lastErr)
	}
	return nil, fmt.Errorf("PrintScreen did not place an image on the clipboard (this PC may open Snipping Tool instead)")
}

func tryOpenClipboard() bool {
	r, _, _ := procOpenClipboard.Call(0)
	return r != 0
}

func openClipboard() error {
	for i := 0; i < 20; i++ {
		if tryOpenClipboard() {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("OpenClipboard failed")
}

func readClipboardDIB() ([]byte, error) {
	if !tryOpenClipboard() {
		return nil, fmt.Errorf("clipboard busy")
	}
	defer procCloseClipboard.Call()

	handle, _, _ := procGetClipboardData.Call(CF_DIB)
	if handle == 0 {
		handle, _, _ = procGetClipboardData.Call(CF_DIBV5)
	}
	if handle == 0 {
		return nil, fmt.Errorf("clipboard has no DIB")
	}
	return copyGlobal(handle)
}

func copyGlobal(handle uintptr) ([]byte, error) {
	size, _, _ := procGlobalSize.Call(handle)
	if size == 0 {
		return nil, fmt.Errorf("clipboard data is empty")
	}
	ptr, _, _ := procGlobalLock.Call(handle)
	if ptr == 0 {
		return nil, fmt.Errorf("GlobalLock failed")
	}
	defer procGlobalUnlock.Call(handle)

	buf := make([]byte, size)
	copy(buf, unsafe.Slice((*byte)(unsafe.Pointer(ptr)), size))
	return buf, nil
}

func restoreClipboard(items []clipboardFormat) error {
	if err := openClipboard(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()

	if r, _, _ := procEmptyClipboard.Call(); r == 0 {
		return fmt.Errorf("EmptyClipboard failed")
	}

	for _, item := range items {
		handle, _, _ := procGlobalAlloc.Call(GMEM_MOVEABLE, uintptr(len(item.data)))
		if handle == 0 {
			return fmt.Errorf("GlobalAlloc failed")
		}
		ptr, _, _ := procGlobalLock.Call(handle)
		if ptr == 0 {
			procGlobalFree.Call(handle)
			return fmt.Errorf("GlobalLock failed")
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(ptr)), len(item.data)), item.data)
		procGlobalUnlock.Call(handle)

		if r, _, _ := procSetClipboardData.Call(uintptr(item.id), handle); r == 0 {
			procGlobalFree.Call(handle)
			return fmt.Errorf("SetClipboardData failed for format %d", item.id)
		}
	}
	return nil
}

func cropPrintScreenImage(img image.Image, region image.Rectangle) (image.Image, error) {
	vx, _, _ := procGetSystemMetrics.Call(SM_XVIRTUALSCREEN)
	vy, _, _ := procGetSystemMetrics.Call(SM_YVIRTUALSCREEN)
	vw, _, _ := procGetSystemMetrics.Call(SM_CXVIRTUALSCREEN)
	vh, _, _ := procGetSystemMetrics.Call(SM_CYVIRTUALSCREEN)

	originX := int(int32(vx))
	originY := int(int32(vy))
	virtualW := int(int32(vw))
	virtualH := int(int32(vh))

	bounds := img.Bounds()
	if bounds.Dx() != virtualW || bounds.Dy() != virtualH {
		primaryW, _, _ := procGetSystemMetrics.Call(SM_CXSCREEN)
		primaryH, _, _ := procGetSystemMetrics.Call(SM_CYSCREEN)
		if bounds.Dx() == int(int32(primaryW)) && bounds.Dy() == int(int32(primaryH)) {
			log.Printf("PrintScreen image is the primary screen (%dx%d), not the full virtual screen (%dx%d)", bounds.Dx(), bounds.Dy(), virtualW, virtualH)
			originX, originY = 0, 0
		} else {
			log.Printf("PrintScreen image is %dx%d; virtual screen is %dx%d", bounds.Dx(), bounds.Dy(), virtualW, virtualH)
		}
	}

	return cropScreenImage(img, region, originX, originY)
}

func cropScreenImage(img image.Image, region image.Rectangle, originX, originY int) (image.Image, error) {
	crop := image.Rect(
		region.Min.X-originX,
		region.Min.Y-originY,
		region.Max.X-originX,
		region.Max.Y-originY,
	)
	bounds := img.Bounds()
	if crop.Empty() || !crop.In(bounds) {
		return nil, fmt.Errorf("selected region %v is outside the PrintScreen image (origin %d,%d image %v crop %v)", region, originX, originY, bounds, crop)
	}

	sub, ok := img.(interface {
		SubImage(r image.Rectangle) image.Image
	})
	if !ok {
		return nil, fmt.Errorf("captured image cannot be cropped")
	}
	return sub.SubImage(crop), nil
}

func dibToImage(data []byte) (*image.NRGBA, error) {
	if len(data) < 40 {
		return nil, fmt.Errorf("DIB too small: %d bytes", len(data))
	}

	biSize := binary.LittleEndian.Uint32(data[0:4])
	if biSize < 40 || int(biSize) > len(data) {
		return nil, fmt.Errorf("invalid DIB header size %d", biSize)
	}

	width := int(int32(binary.LittleEndian.Uint32(data[4:8])))
	heightRaw := int32(binary.LittleEndian.Uint32(data[8:12]))
	planes := binary.LittleEndian.Uint16(data[12:14])
	bitCount := binary.LittleEndian.Uint16(data[14:16])
	compression := binary.LittleEndian.Uint32(data[16:20])

	if planes != 1 {
		return nil, fmt.Errorf("unsupported DIB planes %d", planes)
	}
	if width <= 0 || heightRaw == 0 {
		return nil, fmt.Errorf("invalid DIB size %dx%d", width, heightRaw)
	}

	topDown := heightRaw < 0
	height := int(heightRaw)
	if topDown {
		height = int(-heightRaw)
	}
	if bitCount != 24 && bitCount != 32 {
		return nil, fmt.Errorf("unsupported DIB bit count %d", bitCount)
	}
	if compression != BI_RGB && compression != BI_BITFIELDS {
		return nil, fmt.Errorf("unsupported DIB compression %d", compression)
	}

	pixelOffset := int(biSize)
	if biSize == 40 && compression == BI_BITFIELDS {
		pixelOffset += 12
	}

	bpp := int(bitCount) / 8
	stride := ((width*bpp + 3) / 4) * 4
	need := pixelOffset + stride*height
	if pixelOffset < 0 || stride <= 0 || need > len(data) {
		return nil, fmt.Errorf("truncated DIB: need %d bytes, have %d", need, len(data))
	}

	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		srcY := y
		if !topDown {
			srcY = height - 1 - y
		}
		src := data[pixelOffset+srcY*stride:]
		dst := img.Pix[y*img.Stride:]
		if bitCount == 32 {
			for x := 0; x < width; x++ {
				si := x * 4
				di := x * 4
				dst[di] = src[si+2]
				dst[di+1] = src[si+1]
				dst[di+2] = src[si]
				dst[di+3] = 255
			}
			continue
		}
		for x := 0; x < width; x++ {
			si := x * 3
			di := x * 4
			dst[di] = src[si+2]
			dst[di+1] = src[si+1]
			dst[di+2] = src[si]
			dst[di+3] = 255
		}
	}
	return img, nil
}
