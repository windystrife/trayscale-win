//go:build windows

package wintray

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32          = windows.NewLazySystemDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
)

const (
	mbOK            = 0x00000000
	mbIconInfo      = 0x00000040
	mbSetForeground = 0x00010000
	mbTopMost       = 0x00040000
)

// messageBox shows a simple modal Windows message box. It is safe to call from
// any goroutine; it runs on its own thread so it never blocks the tray.
func messageBox(title, text string) {
	go func() {
		t, err := windows.UTF16PtrFromString(text)
		if err != nil {
			return
		}
		c, err := windows.UTF16PtrFromString(title)
		if err != nil {
			return
		}
		procMessageBoxW.Call(
			0,
			uintptr(unsafe.Pointer(t)),
			uintptr(unsafe.Pointer(c)),
			uintptr(mbOK|mbIconInfo|mbSetForeground|mbTopMost),
		)
	}()
}
