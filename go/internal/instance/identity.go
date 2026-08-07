package instance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Info struct {
	ID           string
	WorkflowID   string
	WorkflowPath string
	DataDir      string
	LockPath     string
}

func Resolve(workflowPath, trackerScope, explicitDataDir string) (Info, error) {
	canonicalPath, err := canonicalWorkflowPath(workflowPath)
	if err != nil {
		return Info{}, err
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return Info{}, fmt.Errorf("resolve user configuration directory: %w", err)
	}

	wid := workflowID(canonicalPath)
	id := instanceID(canonicalPath, normalizeTrackerScope(trackerScope))
	dataDir := filepath.Join(configDir, "Symphony", "instances", id)
	if explicitDataDir != "" {
		dataDir, err = filepath.Abs(explicitDataDir)
		if err != nil {
			return Info{}, fmt.Errorf("resolve data directory: %w", err)
		}
		dataDir = filepath.Clean(dataDir)
	}

	return Info{
		ID:           id,
		WorkflowID:   wid,
		WorkflowPath: canonicalPath,
		DataDir:      dataDir,
		LockPath:     filepath.Join(configDir, "Symphony", "locks", wid+".lock"),
	}, nil
}

func canonicalWorkflowPath(path string) (string, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve workflow path: %w", err)
	}
	absolutePath = filepath.Clean(absolutePath)

	if _, err := os.Lstat(absolutePath); err == nil {
		canonicalPath, err := filepath.EvalSymlinks(absolutePath)
		if err != nil {
			return "", fmt.Errorf("resolve workflow symlinks: %w", err)
		}
		return filepath.Clean(canonicalPath), nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect workflow path: %w", err)
	}

	parent := filepath.Dir(absolutePath)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return "", fmt.Errorf("resolve workflow parent: %w", err)
	}
	if !parentInfo.IsDir() {
		return "", fmt.Errorf("resolve workflow parent %q: not a directory", parent)
	}
	canonicalParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve workflow parent symlinks: %w", err)
	}
	return filepath.Join(canonicalParent, filepath.Base(absolutePath)), nil
}

func normalizeTrackerScope(scope string) string {
	return strings.ToLower(strings.TrimSpace(scope))
}

func workflowID(canonicalPath string) string {
	digest := sha256.Sum256([]byte(canonicalPath))
	return hex.EncodeToString(digest[:16])
}

func instanceID(canonicalPath, normalizedTrackerScope string) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(canonicalPath))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(normalizedTrackerScope))
	return hex.EncodeToString(digest.Sum(nil))
}
