package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// injectDirectory copies only regular files, confining guest writes with os.Root.
func injectDirectory(device, mountPoint, source string, undo *rollback) error {
	if err := mountReadWrite(device, mountPoint); err != nil {
		return err
	}
	undo.push("mount "+mountPoint, func() error { return unmount(mountPoint) })
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("inject source must contain regular files: %s", path)
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return writeGuestFile(mountPoint, "/"+filepath.ToSlash(relative), data, info.Mode().Perm())
	})
	if err != nil {
		return err
	}
	syncFilesystems()
	return unmount(mountPoint)
}
