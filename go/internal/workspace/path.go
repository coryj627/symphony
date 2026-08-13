package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrOutsideRoot           = errors.New("workspace_outside_root")
	ErrRootIdentity          = errors.New("workspace_root_identity_changed")
	ErrExistingNonDirectory  = errors.New("workspace_existing_non_directory")
	ErrWorkspaceKeyCollision = errors.New("workspace_key_collision")
	ErrAmbiguousPath         = errors.New("workspace_ambiguous_path")
)

func prepareRoot(path string) (string, string, error) {
	if strings.TrimSpace(path) == "" {
		return "", "", fmt.Errorf("%w: root is blank", ErrRootIdentity)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace root: %w", err)
	}
	absolute = filepath.Clean(absolute)

	if _, err := os.Lstat(absolute); err == nil {
		return inspectPreparedRoot(absolute)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", "", fmt.Errorf("inspect workspace root: %w", err)
	}

	existing, missing, err := deepestExistingParent(absolute)
	if err != nil {
		return "", "", err
	}
	canonicalParent, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace root parent symlinks: %w", err)
	}
	canonicalParent, err = canonicalExistingPath(canonicalParent)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace root parent: %w", err)
	}
	candidate := filepath.Join(append([]string{canonicalParent}, missing...)...)
	if err := os.MkdirAll(candidate, 0o700); err != nil {
		return "", "", fmt.Errorf("create workspace root: %w", err)
	}
	if err := os.Chmod(candidate, 0o700); err != nil {
		return "", "", fmt.Errorf("secure workspace root: %w", err)
	}
	canonical, identity, err := inspectPreparedRoot(candidate)
	if err != nil {
		return "", "", err
	}
	if candidate != canonicalParent && !pathWithin(canonicalParent, canonical) {
		return "", "", fmt.Errorf("%w: created root resolved outside its parent", ErrOutsideRoot)
	}
	return canonical, identity, nil
}

func inspectPreparedRoot(path string) (string, string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace root symlinks: %w", err)
	}
	resolved, err = canonicalExistingPath(resolved)
	if err != nil {
		return "", "", fmt.Errorf("resolve canonical workspace root: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", "", fmt.Errorf("inspect canonical workspace root: %w", err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("%w: root is not a directory", ErrRootIdentity)
	}
	identity, err := fileIdentity(resolved)
	if err != nil {
		return "", "", fmt.Errorf("identify workspace root: %w", err)
	}
	return filepath.Clean(resolved), identity, nil
}

func deepestExistingParent(path string) (string, []string, error) {
	current := path
	missing := []string{}
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return "", nil, fmt.Errorf("%w: workspace root ancestor is not a directory", ErrRootIdentity)
			}
			return current, missing, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, fmt.Errorf("inspect workspace root ancestor: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", nil, fmt.Errorf("%w: no existing workspace root ancestor", ErrRootIdentity)
		}
		missing = append([]string{filepath.Base(current)}, missing...)
		current = parent
	}
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." || filepath.IsAbs(relative) {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func inspectChildPath(root, candidate string) (os.FileInfo, error) {
	if !pathWithin(root, candidate) {
		return nil, fmt.Errorf("%w: %s", ErrOutsideRoot, candidate)
	}
	info, err := os.Lstat(candidate)
	if err != nil {
		return nil, err
	}
	reparse, err := pathIsReparse(candidate, info)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace path: %w", err)
	}
	if reparse {
		resolved, resolveErr := filepath.EvalSymlinks(candidate)
		if resolveErr == nil {
			resolved, resolveErr = canonicalExistingPath(resolved)
		}
		if resolveErr == nil && !pathWithin(root, resolved) {
			return nil, fmt.Errorf("%w: workspace path resolves to %s", ErrOutsideRoot, resolved)
		}
		return nil, fmt.Errorf("%w: workspace path is a symlink or reparse point", ErrAmbiguousPath)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrExistingNonDirectory, candidate)
	}
	return info, nil
}
