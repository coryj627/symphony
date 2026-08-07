package instance

import (
	"errors"
	"fmt"
	"os"
)

func createMetadataTemp(directory string) (*os.File, string, error) {
	file, err := os.CreateTemp(directory, ".instance.json-*")
	if err != nil {
		return nil, "", err
	}
	if err := file.Chmod(0o600); err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(file.Name())
		return nil, "", errors.Join(fmt.Errorf("secure temporary metadata: %w", err), closeErr, removeErr)
	}
	return file, file.Name(), nil
}

func replaceMetadata(temporaryPath, metadataPath string) error {
	return os.Rename(temporaryPath, metadataPath)
}
