package main

import (
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
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
	keyboardHook  uintptr
	overlay       *OverlayWindow
}

func main() {
	app := &CaptureApp{
		counter: 1,
	}

	fmt.Println("=== Windows Screen Capture Tool ===")
	fmt.Println("Instructions:")
	fmt.Println("1. Press Ctrl + Drag mouse to select/update capture region")
	fmt.Println("2. Press SPACE to capture screenshot")
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

func (app *CaptureApp) captureScreen() error {
	app.mu.Lock()
	defer app.mu.Unlock()

	if app.captureRegion.Empty() {
		return fmt.Errorf("no capture region selected")
	}

	// Capture the specified region
	img, err := screenshot.CaptureRect(app.captureRegion)
	if err != nil {
		return fmt.Errorf("failed to capture screenshot: %v", err)
	}

	// Generate filename
	filename := fmt.Sprintf("capture_%03d.png", app.counter)
	app.counter++

	// Save to file
	file, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to create file: %v", err)
	}
	defer file.Close()

	if err := png.Encode(file, img); err != nil {
		return fmt.Errorf("failed to encode PNG: %v", err)
	}

	absPath, _ := filepath.Abs(filename)
	fmt.Printf("Screenshot saved: %s\n", absPath)
	return nil
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
	hookCallback := func(nCode int, wParam uintptr, lParam uintptr) uintptr {
		if nCode >= 0 {
			kbdStruct := (*KBDLLHOOKSTRUCT)(unsafe.Pointer(lParam))

			if wParam == WM_KEYDOWN {
				switch kbdStruct.VkCode {
				case VK_SPACE:
					// Capture screenshot
					if err := app.captureScreen(); err != nil {
						log.Printf("Error: %v\n", err)
					}
					// Block the space key from passing through
					return 1

				case VK_ESCAPE:
					// Exit the application
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
