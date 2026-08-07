//go:build darwin

package workflow

import "os"

func replaceFile(temporary, destination string) error {
	return os.Rename(temporary, destination)
}

func syncParentDirectory(directory string) error {
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		_ = parent.Close()
		return err
	}
	return parent.Close()
}
