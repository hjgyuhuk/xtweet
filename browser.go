package main

import (
	"os/exec"
	"runtime"
)

func openBrowserActual(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default: // linux, bsd, etc.
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
