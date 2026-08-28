package server

import (
	"io/fs"
	"os"
	"path/filepath"
)

// cacheBytes sums the regular files under the image cache. It is a health
// metric only: a failure (e.g. the directory is missing) reports zero
// instead of failing the Health RPC.
func cacheBytes(stateRoot string) int64 {
	var total int64
	root := filepath.Join(stateRoot, "images")
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		info, err := os.Stat(path)
		if err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
