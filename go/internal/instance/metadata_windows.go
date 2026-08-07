package instance

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func createMetadataTemp(directory string) (*os.File, string, error) {
	for range 100 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", fmt.Errorf("generate temporary metadata name: %w", err)
		}
		path := filepath.Join(directory, ".instance.json-"+hex.EncodeToString(random[:]))
		pathPointer, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, "", err
		}
		handle, err := windows.CreateFile(
			pathPointer,
			windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil,
			windows.CREATE_NEW,
			windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
			0,
		)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, "", err
		}

		file := os.NewFile(uintptr(handle), path)
		if file == nil {
			_ = windows.CloseHandle(handle)
			_ = os.Remove(path)
			return nil, "", fmt.Errorf("create temporary metadata file handle")
		}
		return file, path, nil
	}
	return nil, "", fmt.Errorf("create unique temporary metadata file: too many collisions")
}

func replaceMetadata(temporaryPath, metadataPath string) error {
	temporaryPointer, err := windows.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return err
	}
	metadataPointer, err := windows.UTF16PtrFromString(metadataPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		temporaryPointer,
		metadataPointer,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}
