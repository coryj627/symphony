package instance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func canonicalExistingPath(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	buffer := make([]uint16, 256)
	const volumeNameDOS = 0
	for {
		length, err := windows.GetFinalPathNameByHandle(
			windows.Handle(file.Fd()),
			&buffer[0],
			uint32(len(buffer)),
			volumeNameDOS,
		)
		if err != nil {
			return "", fmt.Errorf("GetFinalPathNameByHandle: %w", err)
		}
		if length < uint32(len(buffer)) {
			resolved := windows.UTF16ToString(buffer[:length])
			resolved = strings.TrimPrefix(resolved, `\\?\`)
			if strings.HasPrefix(resolved, `UNC\`) {
				resolved = `\\` + strings.TrimPrefix(resolved, `UNC\`)
			}
			return filepath.Clean(resolved), nil
		}
		buffer = make([]uint16, length+1)
	}
}
