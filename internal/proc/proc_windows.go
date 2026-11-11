//go:build windows

package proc

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// hideWindow: CREATE_NO_WINDOW — консольная программа запускается без окна; вывод по-прежнему можно читать через каналы.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}
