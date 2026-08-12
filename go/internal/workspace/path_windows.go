package workspace

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
	for {
		length, err := windows.GetFinalPathNameByHandle(windows.Handle(file.Fd()), &buffer[0], uint32(len(buffer)), 0)
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

func fileIdentity(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("%08x:%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}

func pathIsReparse(path string, _ os.FileInfo) (bool, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes, err := windows.GetFileAttributes(pointer)
	if err != nil {
		return false, err
	}
	return attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}

func publishMarker(temporaryPath, markerPath string) error {
	temporary, err := windows.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return err
	}
	marker, err := windows.UTF16PtrFromString(markerPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(temporary, marker, windows.MOVEFILE_WRITE_THROUGH)
}

func syncDirectory(string) error { return nil }
