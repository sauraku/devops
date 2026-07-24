package composepolicy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const maxComposeEnvSize = 4 << 20

const (
	SnapshotComposeName = "compose.yml"
	SnapshotEnvName     = "project.env"
)

// SnapshotInputs copies one repository-controlled Compose file and its
// generated dotenv into an already-created private directory. Both files are
// opened without following their final symlink and are checked against the
// project root before and after opening. The returned files are immutable from
// the project's point of view and are the only inputs that may be passed to
// Docker Compose after this function succeeds.
func SnapshotInputs(composePath, envPath, projectRoot, destinationDir string) (string, string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve project root: %w", err)
	}
	resolvedDestination, err := filepath.EvalSymlinks(destinationDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve Compose snapshot directory: %w", err)
	}
	destinationInfo, err := os.Stat(resolvedDestination)
	if err != nil {
		return "", "", fmt.Errorf("inspect Compose snapshot directory: %w", err)
	}
	if !destinationInfo.IsDir() {
		return "", "", fmt.Errorf("Compose snapshot destination is not a directory")
	}
	if destinationInfo.Mode().Perm()&0o077 != 0 {
		return "", "", fmt.Errorf("Compose snapshot directory must not be accessible by group or other users")
	}
	if pathWithinRoot(resolvedDestination, resolvedRoot) {
		return "", "", fmt.Errorf("Compose snapshot directory must be outside the project root")
	}
	entries, err := os.ReadDir(resolvedDestination)
	if err != nil {
		return "", "", fmt.Errorf("inspect Compose snapshot directory: %w", err)
	}
	if len(entries) != 0 {
		return "", "", fmt.Errorf("Compose snapshot directory must be empty")
	}

	composeSnapshot := filepath.Join(resolvedDestination, SnapshotComposeName)
	if err := snapshotRegularFile(composePath, resolvedRoot, composeSnapshot, maxComposeSourceSize); err != nil {
		return "", "", fmt.Errorf("snapshot Compose source: %w", err)
	}
	if err := ValidateSource(composeSnapshot, resolvedDestination); err != nil {
		return "", "", err
	}

	envSnapshot := ""
	if envPath != "" {
		envSnapshot = filepath.Join(resolvedDestination, SnapshotEnvName)
		if err := snapshotRegularFile(envPath, resolvedRoot, envSnapshot, maxComposeEnvSize); err != nil {
			return "", "", fmt.Errorf("snapshot Compose environment: %w", err)
		}
	}
	return composeSnapshot, envSnapshot, nil
}

func snapshotRegularFile(sourcePath, resolvedRoot, destinationPath string, limit int64) error {
	resolvedSource, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", sourcePath, err)
	}
	if !pathWithinRoot(resolvedSource, resolvedRoot) {
		return fmt.Errorf("%s resolves outside the project root", sourcePath)
	}

	fd, err := unix.Open(resolvedSource, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open %s without following symlinks: %w", sourcePath, err)
	}
	source := os.NewFile(uintptr(fd), resolvedSource)
	if source == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("open %s: invalid file descriptor", sourcePath)
	}
	defer source.Close()

	sourceInfo, err := source.Stat()
	if err != nil {
		return fmt.Errorf("inspect %s: %w", sourcePath, err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", sourcePath)
	}
	if sourceInfo.Size() > limit {
		return fmt.Errorf("%s exceeds %d bytes", sourcePath, limit)
	}

	// Confirm that the name still resolves to the inode that was opened. Once
	// this succeeds, subsequent renames cannot change the file descriptor from
	// which the private snapshot is copied.
	resolvedAgain, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return fmt.Errorf("re-resolve %s: %w", sourcePath, err)
	}
	if !pathWithinRoot(resolvedAgain, resolvedRoot) {
		return fmt.Errorf("%s changed to resolve outside the project root", sourcePath)
	}
	currentInfo, err := os.Stat(resolvedAgain)
	if err != nil {
		return fmt.Errorf("re-inspect %s: %w", sourcePath, err)
	}
	if !os.SameFile(sourceInfo, currentInfo) {
		return fmt.Errorf("%s changed while it was being opened", sourcePath)
	}

	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private snapshot: %w", err)
	}
	copyErr := error(nil)
	written, err := io.Copy(destination, io.LimitReader(source, limit+1))
	if err != nil {
		copyErr = fmt.Errorf("copy private snapshot: %w", err)
	} else if written > limit {
		copyErr = fmt.Errorf("%s exceeds %d bytes", sourcePath, limit)
	}
	if closeErr := destination.Close(); copyErr == nil && closeErr != nil {
		copyErr = fmt.Errorf("close private snapshot: %w", closeErr)
	}
	if copyErr != nil {
		_ = os.Remove(destinationPath)
		return copyErr
	}
	return nil
}

func pathWithinRoot(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil &&
		relative != ".." &&
		!filepath.IsAbs(relative) &&
		!startsWithParentTraversal(relative)
}

func startsWithParentTraversal(relative string) bool {
	return len(relative) > 3 &&
		relative[:3] == ".."+string(filepath.Separator)
}
