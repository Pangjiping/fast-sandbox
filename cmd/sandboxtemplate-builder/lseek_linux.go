//go:build linux

package main

// lseek(2) SEEK_DATA/SEEK_HOLE constants — Linux: DATA=3, HOLE=4.
// (macOS uses the swapped values; see lseek_darwin.go.)
const (
	seekDataConst = 3
	seekHoleConst = 4
)
