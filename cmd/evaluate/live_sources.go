package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type sourceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func fileBytesDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readLimitedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open input file")
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("input must be a regular file")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(data)) > limit {
		return nil, errors.New("input read failed or exceeded limit")
	}
	return data, nil
}

func sourceFingerprint(root string) ([]sourceFile, error) {
	paths := []string{"go.mod", "go.sum"}
	for _, directory := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(
			filepath.Join(root, directory),
			func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return errors.New("cannot enumerate source tree")
				}
				if entry.Type()&os.ModeSymlink != 0 {
					return errors.New("source tree must not contain symbolic links")
				}
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
					return nil
				}
				relative, err := filepath.Rel(root, path)
				if err != nil {
					return errors.New("cannot resolve source path")
				}
				paths = append(paths, filepath.ToSlash(relative))
				if len(paths) > 10000 {
					return errors.New("too many source files")
				}
				return nil
			},
		)
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)

	files := make([]sourceFile, 0, len(paths))
	total := 0
	for _, relative := range paths {
		data, err := readLimitedFile(filepath.Join(root, relative), 8<<20)
		if err != nil {
			return nil, err
		}
		total += len(data)
		if total > 64<<20 {
			return nil, errors.New("source fingerprint exceeds byte budget")
		}
		if relative == "go.mod" {
			fields := strings.Fields(string(data))
			if len(fields) < 2 || fields[0] != "module" || fields[1] != "video-support-agent" {
				return nil, errors.New("source root is not video-support-agent")
			}
		}
		files = append(files, sourceFile{
			Path: relative, SHA256: fileBytesDigest(data),
		})
	}
	return files, nil
}
