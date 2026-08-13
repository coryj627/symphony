package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	markerFilename = ".symphony-workspace.json"
	markerVersion  = 1
	maxMarkerBytes = 16 << 10
)

type ownershipMarker struct {
	Version           int    `json:"version"`
	IssueID           string `json:"issue_id"`
	Identifier        string `json:"identifier"`
	Key               string `json:"workspace_key"`
	RootIdentity      string `json:"root_identity"`
	WorkspaceIdentity string `json:"workspace_identity"`
}

type ownershipMarkerReadOperations struct {
	open  func(string) (*os.File, error)
	lstat func(string) (os.FileInfo, error)
}

func defaultOwnershipMarkerReadOperations() ownershipMarkerReadOperations {
	return ownershipMarkerReadOperations{open: os.Open, lstat: os.Lstat}
}

func writeOwnershipMarker(path string, marker ownershipMarker) (resultErr error) {
	contents, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encode workspace ownership marker: %w", err)
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(path, ".symphony-workspace-*")
	if err != nil {
		return fmt.Errorf("create workspace ownership marker: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		var closeErr error
		if !closed {
			closeErr = temporary.Close()
		}
		removeErr := os.Remove(temporaryPath)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
		resultErr = errors.Join(resultErr, closeErr, removeErr)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure workspace ownership marker: %w", err)
	}
	written, err := temporary.Write(contents)
	if err != nil {
		return fmt.Errorf("write workspace ownership marker: %w", err)
	}
	if written != len(contents) {
		return fmt.Errorf("write workspace ownership marker: %w", io.ErrShortWrite)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync workspace ownership marker: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close workspace ownership marker: %w", err)
	}
	closed = true
	if err := publishMarker(temporaryPath, filepath.Join(path, markerFilename)); err != nil {
		return fmt.Errorf("publish workspace ownership marker: %w", err)
	}
	if err := syncDirectory(path); err != nil {
		return fmt.Errorf("sync workspace directory: %w", err)
	}
	return nil
}

func readOwnershipMarker(path string) (ownershipMarker, error) {
	return readOwnershipMarkerWithOperations(path, defaultOwnershipMarkerReadOperations())
}

func readOwnershipMarkerWithOperations(path string, operations ownershipMarkerReadOperations) (ownershipMarker, error) {
	markerPath := filepath.Join(path, markerFilename)
	file, err := operations.open(markerPath)
	if err != nil {
		return ownershipMarker{}, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return ownershipMarker{}, err
	}
	if !openedInfo.Mode().IsRegular() {
		return ownershipMarker{}, fmt.Errorf("%w: ownership marker is not a regular file", ErrAmbiguousPath)
	}
	pathInfo, err := operations.lstat(markerPath)
	if err != nil {
		return ownershipMarker{}, fmt.Errorf("%w: ownership marker path changed after open: %v", ErrAmbiguousPath, err)
	}
	if !pathInfo.Mode().IsRegular() || !os.SameFile(openedInfo, pathInfo) {
		return ownershipMarker{}, fmt.Errorf("%w: ownership marker path changed after open", ErrAmbiguousPath)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxMarkerBytes+1))
	if err != nil {
		return ownershipMarker{}, err
	}
	if len(contents) > maxMarkerBytes {
		return ownershipMarker{}, fmt.Errorf("%w: ownership marker is too large", ErrAmbiguousPath)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var marker ownershipMarker
	if err := decoder.Decode(&marker); err != nil {
		return ownershipMarker{}, fmt.Errorf("%w: invalid ownership marker: %v", ErrAmbiguousPath, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return ownershipMarker{}, fmt.Errorf("%w: invalid ownership marker: %v", ErrAmbiguousPath, err)
	}
	if marker.Version != markerVersion || marker.IssueID == "" || marker.Identifier == "" || marker.Key == "" || marker.RootIdentity == "" || marker.WorkspaceIdentity == "" {
		return ownershipMarker{}, fmt.Errorf("%w: incomplete ownership marker", ErrAmbiguousPath)
	}
	return marker, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}
