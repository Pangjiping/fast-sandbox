package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// stagePackage encodes the rootfs and memory snapshot as OverlayBD LSMT
// layers (overlaybd format only) and returns the layer files plus the
// per-file import durations.
func stagePackage(workdir, rootfs, memory string) ([]string, int64, int64, error) {
	var layers []string
	var importRootfsMs, importMemoryMs int64
	for _, entry := range []struct{ name, source string }{{"rootfs", rootfs}, {"memory", memory}} {
		started := time.Now()
		destination := filepath.Join(workdir, "overlaybd", entry.name)
		if err := os.MkdirAll(destination, 0o755); err != nil {
			return nil, 0, 0, err
		}
		// The importer writes two temp files and renames the committed one;
		// only the intermediate index is left to clean up (data was renamed
		// into commit by the importer itself).
		data := filepath.Join(destination, "layer.data.tmp")
		index := filepath.Join(destination, "layer.index.tmp")
		commit := filepath.Join(destination, "layer.commit.tmp")
		if output, err := exec.Command(overlaybdImportBin, entry.source, data, index, commit).CombinedOutput(); err != nil {
			return nil, 0, 0, fmt.Errorf("overlaybd import %s: %w: %s", entry.name, err, output)
		}
		elapsed := time.Since(started).Milliseconds()
		if entry.name == "rootfs" {
			importRootfsMs = elapsed
		} else {
			importMemoryMs = elapsed
		}
		_ = os.Remove(index)
		layer := filepath.Join(destination, "layer.lsmt")
		if err := os.Rename(commit, layer); err != nil {
			return nil, 0, 0, err
		}
		layers = append(layers, layer)
	}
	return layers, importRootfsMs, importMemoryMs, nil
}
