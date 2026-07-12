package remotefs

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ExportLayout describes how a source directory is exposed below a managed
// mount root. MountPath always uses slash-separated, relative path components
// so it can cross the RemoteFS wire between different operating systems.
type ExportLayout struct {
	RootPath    string
	RelativeCwd string
	MountPath   string
}

// ResolveExportLayout exports home when cwd is inside it, preserving cwd's
// relative position. For a cwd outside home, it safely falls back to exporting
// cwd itself while still preserving cwd's absolute path hierarchy.
func ResolveExportLayout(home, cwd string) (ExportLayout, error) {
	home, err := filepath.Abs(home)
	if err != nil {
		return ExportLayout{}, err
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return ExportLayout{}, err
	}

	root := cwd
	relativeCwd := "."
	if rel, relErr := filepath.Rel(home, cwd); relErr == nil && pathWithinRoot(rel) {
		root = home
		relativeCwd = rel
	}
	mountPath, err := sourcePathHierarchy(root)
	if err != nil {
		return ExportLayout{}, err
	}
	return ExportLayout{RootPath: root, RelativeCwd: relativeCwd, MountPath: mountPath}, nil
}

func pathWithinRoot(rel string) bool {
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func sourcePathHierarchy(source string) (string, error) {
	volume := filepath.VolumeName(source)
	rest := strings.TrimPrefix(source, volume)
	rest = strings.TrimLeft(rest, `/\`)
	parts := make([]string, 0, 2)
	if volume != "" {
		volume = strings.TrimRight(volume, `:/\`)
		if volume != "" {
			parts = append(parts, volume)
		}
	}
	if rest != "" {
		parts = append(parts, filepath.ToSlash(rest))
	}
	if len(parts) == 0 {
		parts = append(parts, "root")
	}
	return CleanMountPath(strings.Join(parts, "/"))
}

// CleanMountPath validates a wire-level mount hierarchy and returns its clean,
// slash-separated representation.
func CleanMountPath(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, `\`) {
		return "", errors.New("remote fs mount path is invalid")
	}
	clean := path.Clean(value)
	if clean == "." || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("remote fs mount path must stay below the session root")
	}
	return clean, nil
}

// MountPathBelow resolves a validated wire-level hierarchy below base.
func MountPathBelow(base, hierarchy string) (string, error) {
	clean, err := CleanMountPath(hierarchy)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, filepath.FromSlash(clean)), nil
}

// WorkspacePathBelow resolves a source cwd relative to its mounted export root.
func WorkspacePathBelow(mountRoot, relativeCwd string) (string, error) {
	if relativeCwd == "" || strings.ContainsRune(relativeCwd, '\x00') || strings.Contains(relativeCwd, `\`) {
		return "", errors.New("remote fs relative cwd is invalid")
	}
	clean := path.Clean(relativeCwd)
	if strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("remote fs relative cwd must stay below the mounted root")
	}
	if clean == "." {
		return mountRoot, nil
	}
	return filepath.Join(mountRoot, filepath.FromSlash(clean)), nil
}

func CurrentExportLayout(cwd string) (ExportLayout, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return ExportLayout{}, err
	}
	return ResolveExportLayout(home, cwd)
}
