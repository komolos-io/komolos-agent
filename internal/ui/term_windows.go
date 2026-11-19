//go:build windows

package ui

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
	procSetOutputCP    = kernel32.NewProc("SetConsoleOutputCP")
)

const enableVirtualTerminalProcessing = 0x0004

func isTerminal(f *os.File) bool {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}

// enableVT turns on ANSI colour handling in the Windows console (on by default
// in Windows Terminal, off in older consoles) and switches output to UTF-8 so
// the banner and status marks render. It reports whether colour is usable.
func enableVT(f *os.File) bool {
	procSetOutputCP.Call(65001)
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	r, _, _ := procSetConsoleMode.Call(f.Fd(), uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}
