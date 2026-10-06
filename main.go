package main

import (
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/kbinani/screenshot"
	"golang.org/x/sys/windows"
)

var (
	user32                  = windows.NewLazyDLL("user32.dll")
	gdi32                   = windows.NewLazyDLL("gdi32.dll")
	procSetWindowsHookEx    = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procGetMessage          = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessage     = user32.NewProc("DispatchMessageW")
	procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
)

const (
	WH_KEYBOARD_LL = 13
	WH_MOUSE_LL    = 14
	WM_KEYDOWN     = 0x0100
	WM_KEYUP       = 0x0101
	VK_SPACE       = 0x20
	VK_ESCAPE      = 0x1B
	VK_CONTROL     = 0x11
	LLKHF_INJECTED = 0x10
)

type KBDLLHOOKSTRUCT struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type MSG struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      POINT
}

type POINT struct {
	X, Y int32
}

type CaptureApp struct {
	captureRegion image.Rectangle
	counter       int
	mu            sync.Mutex
	captureMu     sync.Mutex
	keyboardHook  uintptr
	overlay       *OverlayWindow
	prefix        string
	outputDir     string
	method        string
}

func main() {
	// Parse command-line flags
	prefix := flag.String("prefix", "goCapture", "파일명 prefix (기본값: goCapture)")
	outputDir := flag.String("dir", "./goCapture", "저장 경로 (기본값: ./goCapture)")
	methodFlag := flag.String("method", "gdi", "캡처 방식: gdi (기본) 또는 prtsc (실제 PrintScreen 키 + 클립보드)")
	flag.Parse()

	method, err := normalizeMethod(*methodFlag)
	if err != nil {
		log.Fatal(err)
	}

	// Validate and create output directory if needed
	absOutputDir, err := filepath.Abs(*outputDir)
	if err != nil {
		log.Fatalf("Invalid output directory: %v", err)
	}

	if err := os.MkdirAll(absOutputDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	app := &CaptureApp{
		counter:   1,
		prefix:    *prefix,
		outputDir: absOutputDir,
		method:    method,
	}

	fmt.Println("=== Windows Screen Capture Tool ===")
	fmt.Printf("파일명 prefix: %s\n", app.prefix)
	fmt.Printf("저장 경로: %s\n", app.outputDir)
	fmt.Printf("캡처 방식: %s\n", app.method)
	fmt.Println()
	if app.method == "prtsc" {
		fmt.Println("이 PC처럼 PrtSc가 캡처 도구만 열면 실패할 수 있습니다.")
		fmt.Println()
	}
	fmt.Println("Instructions:")
	fmt.Println("1. Press Ctrl + Drag mouse to select/update capture region")
	fmt.Println("2. Press SPACE to capture screenshot")
	if app.method == "prtsc" {
		fmt.Println("   (PrintScreen key also works as a fallback)")
	}
	fmt.Println("3. Press ESC to exit")
	fmt.Println()

	// Create overlay window
	app.overlay = NewOverlayWindow()
	app.overlay.SetApp(app)
	go app.overlay.Run()

	// Wait a bit for overlay to initialize
	fmt.Println("Overlay ready. Hold Ctrl and drag to select region.")
	fmt.Println()

	// Start keyboard hook
	if err := app.startKeyboardHook(); err != nil {
		log.Fatal(err)
	}
}

func normalizeMethod(method string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "gdi":
		return "gdi", nil
	case "prtsc", "printscreen":
		return "prtsc", nil
	default:
		return "", fmt.Errorf("unknown capture method %q (use gdi or prtsc)", method)
	}
}

func (app *CaptureApp) captureScreen() error {
	// Serializes captures without holding app.mu across Win32 calls.
	// The overlay thread also takes app.mu while dragging.
	app.captureMu.Lock()
	defer app.captureMu.Unlock()

	app.mu.Lock()
	region := app.captureRegion
	method := app.method
	app.mu.Unlock()

	if region.Empty() {
		return fmt.Errorf("no capture region selected")
	}

	if method == "prtsc" {
		return app.captureScreenPrintScreen(region)
	}

	img, err := screenshot.CaptureRect(region)
	if err != nil {
		return fmt.Errorf("failed to capture screenshot: %v", err)
	}
	return app.writePNG(img)
}

func (app *CaptureApp) captureScreenPrintScreen(region image.Rectangle) error {
	if app.overlay != nil {
		app.overlay.SetVisible(false)
		defer app.overlay.SetVisible(true)
		procDwmFlush.Call()
		time.Sleep(80 * time.Millisecond)
	}

	restoreText, _ := snapshotUnicodeText()
	seqNow, _, _ := procGetClipboardSequenceNumber.Call()
	sendPrintScreen()

	data, err := waitForNewClipboardDIB(uint32(seqNow), printScreenTimeout)
	if err != nil {
		return err
	}
	if len(restoreText) > 0 {
		if err := restoreClipboard([]clipboardFormat{{id: CF_UNICODETEXT, data: restoreText}}); err != nil {
			log.Printf("clipboard restore failed: %v", err)
		}
	}

	img, err := dibToImage(data)
	if err != nil {
		return err
	}
	cropped, err := cropPrintScreenImage(img, region)
	if err != nil {
		return err
	}
	return app.writePNG(cropped)
}

func (app *CaptureApp) writePNG(img image.Image) error {
	app.mu.Lock()
	filename := fmt.Sprintf("%s_%03d.png", app.prefix, app.counter)
	app.counter++
	fullPath := filepath.Join(app.outputDir, filename)
	app.mu.Unlock()

	// Save to file
	file, err := os.Create(fullPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %v", err)
	}
	defer file.Close()

	if err := png.Encode(file, img); err != nil {
		return fmt.Errorf("failed to encode PNG: %v", err)
	}

	fmt.Printf("Screenshot saved: %s\n", fullPath)
	return nil
}

func (app *CaptureApp) savePrintScreenFromClipboard(region image.Rectangle, prevSeq uint32, restoreText []byte) {
	defer func() {
		if app.overlay != nil {
			app.overlay.SetVisible(true)
		}
	}()

	app.captureMu.Lock()
	defer app.captureMu.Unlock()

	data, err := waitForNewClipboardDIB(prevSeq, printScreenTimeout)
	if err != nil {
		log.Printf("Error: failed to capture screenshot: %v", err)
		return
	}
	if len(restoreText) > 0 {
		if err := restoreClipboard([]clipboardFormat{{id: CF_UNICODETEXT, data: restoreText}}); err != nil {
			log.Printf("clipboard restore failed: %v", err)
		}
	}

	img, err := dibToImage(data)
	if err != nil {
		log.Printf("Error: failed to capture screenshot: %v", err)
		return
	}
	cropped, err := cropPrintScreenImage(img, region)
	if err != nil {
		log.Printf("Error: failed to capture screenshot: %v", err)
		return
	}
	if err := app.writePNG(cropped); err != nil {
		log.Printf("Error: %v", err)
	}
}

func (app *CaptureApp) updateRegion(rect image.Rectangle) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.captureRegion = rect
	fmt.Printf("Capture region updated: %v\n", rect)
}

func isCtrlPressed() bool {
	ret, _, _ := procGetAsyncKeyState.Call(uintptr(VK_CONTROL))
	return (ret & 0x8000) != 0
}

func (app *CaptureApp) startKeyboardHook() error {
	// The hook returns immediately, so Space auto-repeat would queue extra captures.
	spaceHeld := false
	printScreenHeld := false

	hookCallback := func(nCode int, wParam uintptr, lParam uintptr) uintptr {
		if nCode >= 0 {
			kbdStruct := (*KBDLLHOOKSTRUCT)(unsafe.Pointer(lParam))

			switch kbdStruct.VkCode {
			case VK_SNAPSHOT:
				// Do not inject PrtSc. Pass the physical key through, then crop
				// the clipboard bitmap Windows produced
				if app.method != "prtsc" || kbdStruct.Flags&LLKHF_INJECTED != 0 {
					break
				}
				if wParam == WM_KEYUP {
					printScreenHeld = false
					break
				}
				if wParam != WM_KEYDOWN || printScreenHeld {
					break
				}
				printScreenHeld = true

				app.mu.Lock()
				region := app.captureRegion
				app.mu.Unlock()
				if region.Empty() {
					log.Printf("Error: no capture region selected")
					break
				}

				if app.overlay != nil {
					app.overlay.SetVisible(false)
					procDwmFlush.Call()
				}
				restoreText, _ := snapshotUnicodeText()
				seqNow, _, _ := procGetClipboardSequenceNumber.Call()
				ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
				go app.savePrintScreenFromClipboard(region, uint32(seqNow), restoreText)
				return ret

			case VK_SPACE:
				if wParam == WM_KEYUP {
					spaceHeld = false
					return 1
				}
				if wParam == WM_KEYDOWN {
					if spaceHeld {
						return 1
					}
					spaceHeld = true
					// Leave the hook immediately. PrintScreen injection must be
					// delivered through this hook; doing it inline deadlocks.
					go func() {
						if err := app.captureScreen(); err != nil {
							log.Printf("Error: %v\n", err)
						}
					}()
					return 1
				}

			case VK_ESCAPE:
				if wParam == WM_KEYDOWN {
					fmt.Println("\nExiting...")
					app.cleanup()
					os.Exit(0)
					return 1
				}
			}
		}

		// Pass all other keys to the system
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return ret
	}

	hook, _, err := procSetWindowsHookEx.Call(
		WH_KEYBOARD_LL,
		syscall.NewCallback(hookCallback),
		0,
		0,
	)

	if hook == 0 {
		return fmt.Errorf("failed to set hook: %v", err)
	}

	app.keyboardHook = hook

	// Message loop
	var msg MSG
	for {
		ret, _, _ := procGetMessage.Call(
			uintptr(unsafe.Pointer(&msg)),
			0,
			0,
			0,
		)

		if ret == 0 {
			break
		}

		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}

	return nil
}

func (app *CaptureApp) cleanup() {
	if app.keyboardHook != 0 {
		procUnhookWindowsHookEx.Call(app.keyboardHook)
	}
	if app.overlay != nil {
		app.overlay.Close()
	}
}
