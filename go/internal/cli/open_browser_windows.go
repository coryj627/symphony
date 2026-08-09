//go:build windows

package cli

import "os/exec"

func openProtectedURL(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Run()
}
