package workspace

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

func canonicalExistingPath(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	buffer := make([]byte, 1024)
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), uintptr(syscall.F_GETPATH), uintptr(unsafe.Pointer(&buffer[0])))
	runtime.KeepAlive(file)
	if errno != 0 {
		return "", fmt.Errorf("fcntl F_GETPATH: %w", errno)
	}
	if end := bytes.IndexByte(buffer, 0); end >= 0 {
		buffer = buffer[:end]
	}
	if len(buffer) == 0 {
		return "", fmt.Errorf("fcntl F_GETPATH returned an empty path")
	}
	return filepath.Clean(string(buffer)), nil
}

func fileIdentity(path string) (string, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), nil
}

func pathIsReparse(_ string, info os.FileInfo) (bool, error) {
	return info.Mode()&os.ModeSymlink != 0, nil
}

func publishMarker(temporaryPath, markerPath string) error {
	if err := os.Link(temporaryPath, markerPath); err != nil {
		return err
	}
	return os.Remove(temporaryPath)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
