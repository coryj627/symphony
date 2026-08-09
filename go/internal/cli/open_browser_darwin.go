//go:build darwin

package cli

import "os/exec"

func openProtectedURL(url string) error { return exec.Command("open", url).Run() }
