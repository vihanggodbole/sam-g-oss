package sam

import (
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
)

//go:embed docs/*.md docs/sam-g.example.json docs/sam-g.subscription.example.json example/README.md
var documentationFS embed.FS

// InstallDocumentation publishes this build's user documentation under root,
// in an immutable content-addressed directory. It
// returns that absolute directory for use in Config.DocumentationDir. It never
// replaces an existing path; a conflicting or modified snapshot is an error.
// The library does not call this function from Open or otherwise write files.
func InstallDocumentation(root string) (string, error) {
	return installDocumentation(root, documentationFS)
}

func installDocumentation(root string, source fs.FS) (string, error) {
	files, version, err := collectDocumentation(source)
	if err != nil {
		return "", err
	}
	tree, err := openDocumentationRoot(root)
	if err != nil {
		return "", err
	}
	defer tree.Close()
	root = tree.Name()

	target := "v1-" + version
	if _, err := tree.Lstat(target); err == nil {
		if err := verifyDocumentation(tree, target, files); err != nil {
			return "", fmt.Errorf("existing documentation snapshot %q is not intact: %w", filepath.Join(root, target), err)
		}
		return filepath.Join(root, target), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("inspect documentation snapshot: %w", err)
	}

	temp, err := makeDocumentationTemp(tree, target)
	if err != nil {
		return "", fmt.Errorf("prepare documentation snapshot: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = tree.RemoveAll(temp)
		}
	}()
	tempRoot, err := tree.OpenRoot(temp)
	if err != nil {
		return "", fmt.Errorf("open temporary documentation snapshot: %w", err)
	}
	for _, name := range sortedDocumentationNames(files) {
		if err := writeDocumentationFile(tempRoot, name, files[name]); err != nil {
			_ = tempRoot.Close()
			return "", fmt.Errorf("write bundled documentation %q: %w", name, err)
		}
	}
	dir, err := tempRoot.Open(".")
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	closeErr := tempRoot.Close()
	if err != nil {
		return "", fmt.Errorf("sync temporary documentation snapshot: %w", err)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close temporary documentation snapshot: %w", closeErr)
	}
	if err := tree.Rename(temp, target); err != nil {
		// Another process may have published the same content-addressed snapshot.
		if _, statErr := tree.Lstat(target); statErr == nil {
			if verifyErr := verifyDocumentation(tree, target, files); verifyErr == nil {
				return filepath.Join(root, target), nil
			}
		}
		return "", fmt.Errorf("publish documentation snapshot: %w", err)
	}
	published = true
	dir, err = tree.Open(".")
	if err != nil {
		return "", fmt.Errorf("open documentation directory for sync: %w", err)
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err != nil || closeErr != nil {
		return "", fmt.Errorf("sync documentation directory: %w", errors.Join(err, closeErr))
	}
	return filepath.Join(root, target), nil
}

func openDocumentationRoot(name string) (*os.Root, error) {
	if name == "" {
		return nil, errors.New("documentation directory is required")
	}
	root, err := filepath.Abs(name)
	if err != nil {
		return nil, fmt.Errorf("resolve documentation directory: %w", err)
	}
	info, err := os.Lstat(root)
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return nil, fmt.Errorf("documentation directory %q must be a real directory, not a symlink or file", root)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect documentation directory: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create documentation directory: %w", err)
	}
	info, err = os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect documentation directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("documentation directory %q must be a real directory, not a symlink or file", root)
	}
	tree, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open documentation directory: %w", err)
	}
	return tree, nil
}

func collectDocumentation(source fs.FS) (map[string][]byte, string, error) {
	files := make(map[string][]byte)
	err := fs.WalkDir(source, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." || entry.IsDir() {
			return nil
		}
		if !fs.ValidPath(name) || !filepath.IsLocal(filepath.FromSlash(name)) {
			return fmt.Errorf("unsafe bundled documentation path %q", name)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("bundled documentation path %q is not a regular file", name)
		}
		data, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		files[name] = data
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("read bundled documentation: %w", err)
	}
	if len(files) == 0 {
		return nil, "", errors.New("documentation bundle is empty")
	}
	names := sortedDocumentationNames(files)
	h := sha256.New()
	for _, name := range names {
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(files[name])
		_, _ = h.Write([]byte{0})
	}
	return files, hex.EncodeToString(h.Sum(nil)), nil
}

func sortedDocumentationNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func makeDocumentationTemp(root *os.Root, target string) (string, error) {
	for range 10 {
		name := "." + target + "-" + rand.Text()
		if err := root.Mkdir(name, 0o700); err == nil {
			return name, nil
		} else if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("could not allocate a temporary directory")
}

func writeDocumentationFile(root *os.Root, name string, data []byte) error {
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func verifyDocumentation(root *os.Root, target string, files map[string][]byte) error {
	info, err := root.Lstat(target)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("snapshot is not a real directory")
	}
	snapshot, err := root.OpenRoot(target)
	if err != nil {
		return err
	}
	defer snapshot.Close()
	expectedFiles := make(map[string]bool, len(files))
	expectedDirs := make(map[string]bool)
	for name := range files {
		expectedFiles[name] = true
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			expectedDirs[dir] = true
		}
	}
	seenFiles, seenDirs := map[string]bool{}, map[string]bool{}
	err = fs.WalkDir(snapshot.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if entry.IsDir() {
			if !expectedDirs[name] {
				return fmt.Errorf("unexpected directory %q", name)
			}
			seenDirs[name] = true
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !expectedFiles[name] || !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected or unsafe file %q", name)
		}
		actual, err := fs.ReadFile(snapshot.FS(), name)
		if err != nil {
			return err
		}
		if !slices.Equal(actual, files[name]) {
			return fmt.Errorf("file %q has changed", name)
		}
		seenFiles[name] = true
		return nil
	})
	if err != nil {
		return err
	}
	if len(seenFiles) != len(expectedFiles) || len(seenDirs) != len(expectedDirs) {
		return errors.New("snapshot is incomplete")
	}
	return nil
}
