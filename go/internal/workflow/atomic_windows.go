//go:build windows

package workflow

import "golang.org/x/sys/windows"

func replaceFile(temporary, destination string) error {
	from, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// MoveFileEx with MOVEFILE_WRITE_THROUGH asks Windows to flush the move. There
// is no portable directory-handle fsync equivalent required after it.
func syncParentDirectory(string) error { return nil }
