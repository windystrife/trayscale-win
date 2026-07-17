//go:build windows

package winui

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	comdlg32            = windows.NewLazySystemDLL("comdlg32.dll")
	procGetOpenFileName = comdlg32.NewProc("GetOpenFileNameW")
)

// openFileNameW mirrors the Win32 OPENFILENAMEW struct.
type openFileNameW struct {
	lStructSize       uint32
	hwndOwner         uintptr
	hInstance         uintptr
	lpstrFilter       *uint16
	lpstrCustomFilter *uint16
	nMaxCustFilter    uint32
	nFilterIndex      uint32
	lpstrFile         *uint16
	nMaxFile          uint32
	lpstrFileTitle    *uint16
	nMaxFileTitle     uint32
	lpstrInitialDir   *uint16
	lpstrTitle        *uint16
	flags             uint32
	nFileOffset       uint16
	nFileExtension    uint16
	lpstrDefExt       *uint16
	lCustData         uintptr
	lpfnHook          uintptr
	lpTemplateName    *uint16
	pvReserved        uintptr
	dwReserved        uint32
	flagsEx           uint32
}

const (
	ofnFileMustExist = 0x00001000
	ofnPathMustExist = 0x00000800
	ofnExplorer      = 0x00080000
	ofnNoChangeDir   = 0x00000008
	ofnHideReadOnly  = 0x00000004
)

// openFileDialog shows the native Windows file-open dialog and returns the
// selected path. ok is false if the user cancelled.
func openFileDialog() (path string, ok bool) {
	buf := make([]uint16, 4096)
	title, _ := windows.UTF16PtrFromString("Select a file to send")

	ofn := openFileNameW{
		lStructSize: uint32(unsafe.Sizeof(openFileNameW{})),
		lpstrFile:   &buf[0],
		nMaxFile:    uint32(len(buf)),
		lpstrTitle:  title,
		flags:       ofnFileMustExist | ofnPathMustExist | ofnExplorer | ofnNoChangeDir | ofnHideReadOnly,
	}

	r, _, _ := procGetOpenFileName.Call(uintptr(unsafe.Pointer(&ofn)))
	if r == 0 {
		return "", false // cancelled or error
	}
	return syscall.UTF16ToString(buf), true
}
