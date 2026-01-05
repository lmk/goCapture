package main

import (
	"fmt"
	"image"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shcore                         = windows.NewLazyDLL("shcore.dll")
	procSetProcessDpiAwareness     = shcore.NewProc("SetProcessDpiAwareness")

	procCreateWindowEx             = user32.NewProc("CreateWindowExW")
	procDefWindowProc              = user32.NewProc("DefWindowProcW")
	procPostQuitMessage            = user32.NewProc("PostQuitMessage")
	procRegisterClassEx            = user32.NewProc("RegisterClassExW")
	procShowWindow                 = user32.NewProc("ShowWindow")
	procUpdateWindow               = user32.NewProc("UpdateWindow")
	procDestroyWindow              = user32.NewProc("DestroyWindow")
	procGetDC                      = user32.NewProc("GetDC")
	procReleaseDC                  = user32.NewProc("ReleaseDC")
	procBeginPaint                 = user32.NewProc("BeginPaint")
	procEndPaint                   = user32.NewProc("EndPaint")
	procSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
	procGetSystemMetrics           = user32.NewProc("GetSystemMetrics")
	procSetWindowLong              = user32.NewProc("SetWindowLongW")
	procGetWindowLong              = user32.NewProc("GetWindowLongW")
	procInvalidateRect             = user32.NewProc("InvalidateRect")
	procPostMessage                = user32.NewProc("PostMessageW")
	procGetCursorPos               = user32.NewProc("GetCursorPos")

	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	procDeleteObject     = gdi32.NewProc("DeleteObject")
	procRectangle        = gdi32.NewProc("Rectangle")
	procCreatePen        = gdi32.NewProc("CreatePen")
	procSelectObject     = gdi32.NewProc("SelectObject")
)

const (
	WS_EX_TOPMOST     = 0x00000008
	WS_EX_LAYERED     = 0x00080000
	WS_EX_TRANSPARENT = 0x00000020
	WS_EX_TOOLWINDOW  = 0x00000080
	WS_POPUP          = 0x80000000
	WS_VISIBLE        = 0x10000000
	SW_SHOW           = 5
	WM_DESTROY        = 0x0002
	WM_PAINT          = 0x000F
	WM_LBUTTONDOWN    = 0x0201
	WM_LBUTTONUP      = 0x0202
	WM_MOUSEMOVE      = 0x0200
	WM_CLOSE          = 0x0010
	WM_USER           = 0x0400
	WM_UPDATE_RECT    = WM_USER + 1
	LWA_ALPHA         = 0x00000002
	SM_CXSCREEN       = 0
	SM_CYSCREEN       = 1
	PS_SOLID          = 0
	NULL_BRUSH        = 5
	GWL_EXSTYLE       = -20
	MK_CONTROL        = 0x0008
	VK_LBUTTON        = 0x01
)

type WNDCLASSEX struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type RECT struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type PAINTSTRUCT struct {
	Hdc         uintptr
	FErase      int32
	RcPaint     RECT
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

type MSLLHOOKSTRUCT struct {
	Pt          POINT
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type OverlayWindow struct {
	hwnd       uintptr
	startX     int32
	startY     int32
	endX       int32
	endY       int32
	isDragging bool
	lastRect   image.Rectangle
	app        *CaptureApp
	mouseHook  uintptr
}

var globalOverlay *OverlayWindow

func NewOverlayWindow() *OverlayWindow {
	ow := &OverlayWindow{}
	globalOverlay = ow
	return ow
}

func (ow *OverlayWindow) Run() error {
	// Set DPI awareness to get correct screen coordinates
	procSetProcessDpiAwareness.Call(2) // PROCESS_PER_MONITOR_DPI_AWARE

	className := windows.StringToUTF16Ptr("OverlayWindowClass")

	wc := WNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(WNDCLASSEX{})),
		LpfnWndProc:   syscall.NewCallback(wndProc),
		HInstance:     0,
		HbrBackground: 0,
		LpszClassName: className,
	}

	ret, _, _ := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	if ret == 0 {
		return fmt.Errorf("failed to register window class")
	}

	// Get screen dimensions
	screenWidth, _, _ := procGetSystemMetrics.Call(SM_CXSCREEN)
	screenHeight, _, _ := procGetSystemMetrics.Call(SM_CYSCREEN)

	// Create the overlay window with WS_EX_TRANSPARENT to allow click-through by default
	hwnd, _, _ := procCreateWindowEx.Call(
		WS_EX_TOPMOST|WS_EX_LAYERED|WS_EX_TOOLWINDOW|WS_EX_TRANSPARENT,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Select Region"))),
		WS_POPUP|WS_VISIBLE,
		0, 0,
		screenWidth, screenHeight,
		0, 0, 0, 0,
	)

	if hwnd == 0 {
		return fmt.Errorf("failed to create window")
	}

	ow.hwnd = hwnd

	// Set window transparency (very transparent so barely visible)
	procSetLayeredWindowAttributes.Call(hwnd, 0, 30, LWA_ALPHA)

	procShowWindow.Call(hwnd, SW_SHOW)
	procUpdateWindow.Call(hwnd)

	// Install mouse hook to detect Ctrl+Drag
	if err := ow.installMouseHook(); err != nil {
		return err
	}

	// Message loop
	var msg MSG
	for {
		ret, _, _ := procGetMessage.Call(
			uintptr(unsafe.Pointer(&msg)),
			0, 0, 0,
		)

		if ret == 0 {
			break
		}

		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}

	return nil
}

func (ow *OverlayWindow) installMouseHook() error {
	mouseCallback := func(nCode int, wParam uintptr, lParam uintptr) uintptr {
		if nCode >= 0 && ow != nil {
			mouseStruct := (*MSLLHOOKSTRUCT)(unsafe.Pointer(lParam))

			switch wParam {
			case WM_LBUTTONDOWN:
				// Check if Ctrl is pressed
				if isCtrlPressed() {
					ow.startX = mouseStruct.Pt.X
					ow.startY = mouseStruct.Pt.Y
					ow.endX = ow.startX
					ow.endY = ow.startY
					ow.isDragging = true
					procPostMessage.Call(ow.hwnd, WM_UPDATE_RECT, 0, 0)
				}

			case WM_MOUSEMOVE:
				if ow.isDragging && isCtrlPressed() {
					ow.endX = mouseStruct.Pt.X
					ow.endY = mouseStruct.Pt.Y
					procPostMessage.Call(ow.hwnd, WM_UPDATE_RECT, 0, 0)
				}

			case WM_LBUTTONUP:
				if ow.isDragging {
					ow.endX = mouseStruct.Pt.X
					ow.endY = mouseStruct.Pt.Y
					ow.isDragging = false

					// Calculate the selected rectangle
					minX := min(ow.startX, ow.endX)
					maxX := max(ow.startX, ow.endX)
					minY := min(ow.startY, ow.endY)
					maxY := max(ow.startY, ow.endY)

					rect := image.Rect(int(minX), int(minY), int(maxX), int(maxY))
					ow.lastRect = rect

					// Update the capture region in the app
					if ow.app != nil {
						ow.app.updateRegion(rect)
					}

					procPostMessage.Call(ow.hwnd, WM_UPDATE_RECT, 0, 0)
				}
			}
		}

		// Pass to next hook
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return ret
	}

	hook, _, err := procSetWindowsHookEx.Call(
		WH_MOUSE_LL,
		syscall.NewCallback(mouseCallback),
		0,
		0,
	)

	if hook == 0 {
		return fmt.Errorf("failed to set mouse hook: %v", err)
	}

	ow.mouseHook = hook
	return nil
}

func wndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	ow := globalOverlay
	if ow == nil {
		ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	}

	switch msg {
	case WM_UPDATE_RECT:
		procInvalidateRect.Call(hwnd, 0, 1)
		return 0

	case WM_PAINT:
		// Call BeginPaint/EndPaint to validate the window
		var ps PAINTSTRUCT
		hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

		if !ow.lastRect.Empty() || ow.isDragging {
			// Create a red pen for the rectangle border
			pen, _, _ := procCreatePen.Call(PS_SOLID, 3, 0x0000FF) // Red color (BGR format)
			oldPen, _, _ := procSelectObject.Call(hdc, pen)

			// Select null brush (transparent fill)
			oldBrush, _, _ := procSelectObject.Call(hdc, NULL_BRUSH)

			// Draw the last selected rectangle
			if !ow.lastRect.Empty() {
				procRectangle.Call(hdc,
					uintptr(ow.lastRect.Min.X),
					uintptr(ow.lastRect.Min.Y),
					uintptr(ow.lastRect.Max.X),
					uintptr(ow.lastRect.Max.Y))
			}

			// If currently dragging, draw the current selection
			if ow.isDragging {
				minX := min(ow.startX, ow.endX)
				maxX := max(ow.startX, ow.endX)
				minY := min(ow.startY, ow.endY)
				maxY := max(ow.startY, ow.endY)

				procRectangle.Call(hdc, uintptr(minX), uintptr(minY), uintptr(maxX), uintptr(maxY))
			}

			// Cleanup
			procSelectObject.Call(hdc, oldPen)
			procSelectObject.Call(hdc, oldBrush)
			procDeleteObject.Call(pen)
		}

		procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0

	case WM_CLOSE, WM_DESTROY:
		if ow.mouseHook != 0 {
			procUnhookWindowsHookEx.Call(ow.mouseHook)
		}
		procPostQuitMessage.Call(0)
		return 0

	default:
		ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	}
}

func (ow *OverlayWindow) Close() {
	if ow.hwnd != 0 {
		procPostMessage.Call(ow.hwnd, WM_CLOSE, 0, 0)
	}
}

func (ow *OverlayWindow) SetApp(app *CaptureApp) {
	ow.app = app
}

func min(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func max(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
