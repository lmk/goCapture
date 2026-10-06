package main

import (
	"fmt"
	"image"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shcore                     = windows.NewLazyDLL("shcore.dll")
	procSetProcessDpiAwareness = shcore.NewProc("SetProcessDpiAwareness")

	procCreateWindowEx      = user32.NewProc("CreateWindowExW")
	procDefWindowProc       = user32.NewProc("DefWindowProcW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procRegisterClassEx     = user32.NewProc("RegisterClassExW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procPostMessage         = user32.NewProc("PostMessageW")
	procSetTimer            = user32.NewProc("SetTimer")
	procKillTimer           = user32.NewProc("KillTimer")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procGetDC               = user32.NewProc("GetDC")
	procReleaseDC           = user32.NewProc("ReleaseDC")
	procBeginPaint          = user32.NewProc("BeginPaint")
	procEndPaint            = user32.NewProc("EndPaint")
	procFillRect            = user32.NewProc("FillRect")
	procUpdateLayeredWindow = user32.NewProc("UpdateLayeredWindow")

	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procCreateSolidBrush       = gdi32.NewProc("CreateSolidBrush")
)

const (
	WS_EX_TOPMOST      = 0x00000008
	WS_EX_LAYERED      = 0x00080000
	WS_EX_TRANSPARENT  = 0x00000020
	WS_EX_TOOLWINDOW   = 0x00000080
	WS_EX_NOACTIVATE   = 0x08000000
	WS_POPUP           = 0x80000000
	SW_HIDE            = 0
	SW_SHOWNA          = 8
	WM_DESTROY         = 0x0002
	WM_PAINT           = 0x000F
	WM_ERASEBKGND      = 0x0014
	WM_LBUTTONDOWN     = 0x0201
	WM_LBUTTONUP       = 0x0202
	WM_MOUSEMOVE       = 0x0200
	WM_CLOSE           = 0x0010
	WM_TIMER           = 0x0113
	WM_USER            = 0x0400
	WM_UPDATE_RECT     = WM_USER + 1
	SM_CXSCREEN        = 0
	SM_CYSCREEN        = 1
	SM_XVIRTUALSCREEN  = 76
	SM_YVIRTUALSCREEN  = 77
	SM_CXVIRTUALSCREEN = 78
	SM_CYVIRTUALSCREEN = 79
	AC_SRC_OVER        = 0
	ULW_ALPHA          = 0x00000002
	borderPx           = int32(4)
	overlayTimerID     = 1
	overlayTickMs      = 100
	frameInterval      = uint32(16)
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

type SIZE struct {
	CX int32
	CY int32
}

type BLENDFUNCTION struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

type MSLLHOOKSTRUCT struct {
	Pt          POINT
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type barSurface struct {
	dc, bmp, old uintptr
	w, h         int32
}

type OverlayWindow struct {
	hwnd       uintptr
	bars       [4]uintptr
	surf       [4]barSurface
	barX       [4]int32
	barY       [4]int32
	barW       [4]int32
	barH       [4]int32
	barOn      [4]bool
	app        *CaptureApp
	mouseHook  uintptr
	brush      uintptr
	failLogged bool

	// Hook writes these. The window thread reads them.
	startX       int32
	startY       int32
	endX         int32
	endY         int32
	dragging     int32
	dragCommit   int32
	hidden       int32
	paintPending int32
	lastTick     int64

	hasRect bool
	winX    int32
	winY    int32
	winW    int32
	winH    int32
}

var globalOverlay *OverlayWindow

func NewOverlayWindow() *OverlayWindow {
	ow := &OverlayWindow{}
	globalOverlay = ow
	return ow
}

func (ow *OverlayWindow) Run() error {
	// The low-level mouse hook is bound to this OS thread. If the goroutine
	// migrates, Windows posts hook calls to a thread with no message loop and
	// drops them after a timeout.
	runtime.LockOSThread()

	procSetProcessDpiAwareness.Call(2) // PROCESS_PER_MONITOR_DPI_AWARE

	className := windows.StringToUTF16Ptr("OverlayWindowClass")
	ow.brush, _, _ = procCreateSolidBrush.Call(0x0000FF) // red, COLORREF BGR
	if ow.brush == 0 {
		return fmt.Errorf("failed to create border brush")
	}

	wc := WNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(WNDCLASSEX{})),
		LpfnWndProc:   syscall.NewCallback(wndProc),
		LpszClassName: className,
	}

	ret, _, _ := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	if ret == 0 {
		return fmt.Errorf("failed to register window class")
	}

	// Thin bars only. A fullscreen layered window is what made dragging stall.
	for i := 0; i < 4; i++ {
		hwnd, _, _ := procCreateWindowEx.Call(
			WS_EX_TOPMOST|WS_EX_LAYERED|WS_EX_TOOLWINDOW|WS_EX_TRANSPARENT|WS_EX_NOACTIVATE,
			uintptr(unsafe.Pointer(className)),
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Select Region"))),
			WS_POPUP,
			0, 0, 1, 1,
			0, 0, 0, 0,
		)
		if hwnd == 0 {
			return fmt.Errorf("failed to create border window")
		}
		ow.bars[i] = hwnd
	}

	ow.hwnd = ow.bars[0]
	// Idle safety net for a missed button-up. During a drag this message is
	// starved, so the hook posts WM_UPDATE_RECT itself.
	procSetTimer.Call(ow.hwnd, overlayTimerID, overlayTickMs, 0)

	if err := ow.installMouseHook(); err != nil {
		return err
	}

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
	// Windows waits for this callback before delivering the next mouse event.
	mouseCallback := func(nCode int, wParam uintptr, lParam uintptr) uintptr {
		if nCode >= 0 && ow != nil {
			block := false
			if atomic.LoadInt32(&ow.dragging) != 0 {
				switch wParam {
				case WM_MOUSEMOVE:
					// Swallowing this freezes the cursor. The click is already
					// eaten, so the window below does not start a drag.
					mouseStruct := (*MSLLHOOKSTRUCT)(unsafe.Pointer(lParam))
					atomic.StoreInt32(&ow.endX, mouseStruct.Pt.X)
					atomic.StoreInt32(&ow.endY, mouseStruct.Pt.Y)
					ow.requestFrame(false, mouseStruct.Time)
				case WM_LBUTTONUP:
					mouseStruct := (*MSLLHOOKSTRUCT)(unsafe.Pointer(lParam))
					atomic.StoreInt32(&ow.endX, mouseStruct.Pt.X)
					atomic.StoreInt32(&ow.endY, mouseStruct.Pt.Y)
					atomic.StoreInt32(&ow.dragging, 0)
					atomic.StoreInt32(&ow.dragCommit, 1)
					ow.requestFrame(true, mouseStruct.Time)
					block = true
				}
			} else if wParam == WM_LBUTTONDOWN && isCtrlPressed() {
				mouseStruct := (*MSLLHOOKSTRUCT)(unsafe.Pointer(lParam))
				atomic.StoreInt32(&ow.startX, mouseStruct.Pt.X)
				atomic.StoreInt32(&ow.startY, mouseStruct.Pt.Y)
				atomic.StoreInt32(&ow.endX, mouseStruct.Pt.X)
				atomic.StoreInt32(&ow.endY, mouseStruct.Pt.Y)
				atomic.StoreInt32(&ow.dragging, 1)
				ow.requestFrame(true, mouseStruct.Time)
				block = true
			}
			if block {
				return 1
			}
		}

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

// requestFrame posts one paint. WM_TIMER and WM_PAINT never run while mouse
// hook messages keep the queue non-empty, so the hook has to ask directly.
func (ow *OverlayWindow) requestFrame(force bool, tick uint32) {
	if ow == nil || ow.hwnd == 0 {
		return
	}
	if !force {
		last := uint32(atomic.LoadInt64(&ow.lastTick))
		if tick-last < frameInterval {
			return
		}
	}
	if !atomic.CompareAndSwapInt32(&ow.paintPending, 0, 1) {
		return
	}
	atomic.StoreInt64(&ow.lastTick, int64(tick))
	procPostMessage.Call(ow.hwnd, WM_UPDATE_RECT, 0, 0)
}

func (ow *OverlayWindow) paintNow() {
	if atomic.LoadInt32(&ow.hidden) != 0 {
		return
	}

	rect := screenRect(
		atomic.LoadInt32(&ow.startX),
		atomic.LoadInt32(&ow.startY),
		atomic.LoadInt32(&ow.endX),
		atomic.LoadInt32(&ow.endY),
	)
	if atomic.LoadInt32(&ow.dragging) != 0 {
		ow.applyRect(rect)
		return
	}
	if atomic.CompareAndSwapInt32(&ow.dragCommit, 1, 0) {
		ow.applyRect(rect)
		if ow.app != nil {
			ow.app.updateRegion(rect)
		}
	}
}

func (ow *OverlayWindow) applyRect(r image.Rectangle) {
	w := int32(r.Dx())
	h := int32(r.Dy())
	if w < 2 || h < 2 {
		if ow.hasRect {
			ow.hideBars(true)
			ow.hasRect = false
		}
		return
	}

	x := int32(r.Min.X)
	y := int32(r.Min.Y)
	if ow.hasRect && x == ow.winX && y == ow.winY && w == ow.winW && h == ow.winH {
		return
	}

	b := borderPx
	if w <= b*2 || h <= b*2 {
		ow.placeBar(0, x, y, w, h)
		ow.placeBar(1, 0, 0, 0, 0)
		ow.placeBar(2, 0, 0, 0, 0)
		ow.placeBar(3, 0, 0, 0, 0)
	} else {
		ow.placeBar(0, x, y, w, b)
		ow.placeBar(1, x, y+h-b, w, b)
		ow.placeBar(2, x, y+b, b, h-2*b)
		ow.placeBar(3, x+w-b, y+b, b, h-2*b)
	}
	ow.winX, ow.winY, ow.winW, ow.winH = x, y, w, h
	ow.hasRect = true
}

func (ow *OverlayWindow) placeBar(i int, x, y, w, h int32) {
	hwnd := ow.bars[i]
	if hwnd == 0 {
		return
	}
	if w < 1 || h < 1 {
		if ow.barOn[i] {
			procShowWindow.Call(hwnd, SW_HIDE)
			ow.barOn[i] = false
		}
		return
	}
	if ow.barOn[i] && ow.barX[i] == x && ow.barY[i] == y && ow.barW[i] == w && ow.barH[i] == h {
		return
	}
	if !ow.ensureSurface(i, w, h) {
		return
	}
	if !ow.barOn[i] {
		procShowWindow.Call(hwnd, SW_SHOWNA)
	}
	if !ow.presentBar(hwnd, ow.surf[i].dc, x, y, w, h) {
		return
	}
	ow.barX[i], ow.barY[i], ow.barW[i], ow.barH[i] = x, y, w, h
	ow.barOn[i] = true
}

func (ow *OverlayWindow) ensureSurface(i int, w, h int32) bool {
	s := &ow.surf[i]
	if s.dc != 0 && s.w == w && s.h == h {
		return true
	}
	ow.releaseSurface(i)

	screen, _, _ := procGetDC.Call(0)
	if screen == 0 {
		ow.logFail(fmt.Errorf("GetDC failed"))
		return false
	}
	defer procReleaseDC.Call(0, screen)

	s.dc, _, _ = procCreateCompatibleDC.Call(screen)
	s.bmp, _, _ = procCreateCompatibleBitmap.Call(screen, uintptr(w), uintptr(h))
	if s.dc == 0 || s.bmp == 0 {
		ow.logFail(fmt.Errorf("border bitmap failed"))
		ow.releaseSurface(i)
		return false
	}
	s.old, _, _ = procSelectObject.Call(s.dc, s.bmp)
	rc := RECT{Right: w, Bottom: h}
	procFillRect.Call(s.dc, uintptr(unsafe.Pointer(&rc)), ow.brush)
	s.w, s.h = w, h
	return true
}

func (ow *OverlayWindow) releaseSurface(i int) {
	s := &ow.surf[i]
	if s.dc == 0 {
		return
	}
	if s.old != 0 {
		procSelectObject.Call(s.dc, s.old)
	}
	if s.bmp != 0 {
		procDeleteObject.Call(s.bmp)
	}
	procDeleteDC.Call(s.dc)
	*s = barSurface{}
}

func (ow *OverlayWindow) presentBar(hwnd, src uintptr, x, y, w, h int32) bool {
	dst := POINT{X: x, Y: y}
	size := SIZE{CX: w, CY: h}
	srcPt := POINT{}
	blend := BLENDFUNCTION{
		BlendOp:             AC_SRC_OVER,
		SourceConstantAlpha: 255,
	}
	ret, _, err := procUpdateLayeredWindow.Call(
		hwnd,
		0,
		uintptr(unsafe.Pointer(&dst)),
		uintptr(unsafe.Pointer(&size)),
		src,
		uintptr(unsafe.Pointer(&srcPt)),
		0,
		uintptr(unsafe.Pointer(&blend)),
		ULW_ALPHA,
	)
	if ret == 0 {
		ow.logFail(err)
		return false
	}
	return true
}

func (ow *OverlayWindow) logFail(err error) {
	if ow.failLogged {
		return
	}
	ow.failLogged = true
	fmt.Printf("border update failed: %v\n", err)
}

func wndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	ow := globalOverlay
	if ow == nil {
		ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	}

	switch msg {
	case WM_ERASEBKGND:
		return 1

	case WM_PAINT:
		var ps PAINTSTRUCT
		procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0

	case WM_UPDATE_RECT:
		atomic.StoreInt32(&ow.paintPending, 0)
		ow.paintNow()
		return 0

	case WM_TIMER:
		if wParam == overlayTimerID && hwnd == ow.hwnd {
			if atomic.LoadInt32(&ow.dragging) != 0 || atomic.LoadInt32(&ow.dragCommit) != 0 {
				ow.paintNow()
			}
		}
		return 0

	case WM_CLOSE, WM_DESTROY:
		procKillTimer.Call(hwnd, overlayTimerID)
		if ow.mouseHook != 0 {
			procUnhookWindowsHookEx.Call(ow.mouseHook)
			ow.mouseHook = 0
		}
		for i := range ow.surf {
			ow.releaseSurface(i)
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

// SetVisible hides or shows the selection border.
// PrintScreen captures topmost windows, so the border is hidden during that capture.
func (ow *OverlayWindow) SetVisible(visible bool) {
	if ow == nil || ow.hwnd == 0 {
		return
	}
	if visible {
		atomic.StoreInt32(&ow.hidden, 0)
		if ow.hasRect {
			x, y, w, h := ow.winX, ow.winY, ow.winW, ow.winH
			for i := range ow.barOn {
				ow.barOn[i] = false
			}
			ow.winW = -1
			ow.applyRect(image.Rect(int(x), int(y), int(x+w), int(y+h)))
		}
		return
	}
	atomic.StoreInt32(&ow.hidden, 1)
	ow.hideBars(false)
}

func (ow *OverlayWindow) hideBars(drop bool) {
	for i, hwnd := range ow.bars {
		if hwnd != 0 {
			procShowWindow.Call(hwnd, SW_HIDE)
		}
		if drop {
			ow.barOn[i] = false
		}
	}
}

func screenRect(x1, y1, x2, y2 int32) image.Rectangle {
	if x1 > x2 {
		x1, x2 = x2, x1
	}
	if y1 > y2 {
		y1, y2 = y2, y1
	}
	return image.Rect(int(x1), int(y1), int(x2), int(y2))
}
