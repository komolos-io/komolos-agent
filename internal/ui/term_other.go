//go:build !windows

package ui

import "os"

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func enableVT(*os.File) bool { return true }
