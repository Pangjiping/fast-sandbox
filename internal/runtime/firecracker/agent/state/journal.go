package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// journalEntry is the wire shape of both line kinds: an intent carries
// Args (and no Result), a result carries Result (and no Args). The pair is
// matched by RequestID.
type journalEntry struct {
	RequestID string          `json:"requestId"`
	Op        string          `json:"op"`
	PodUID    string          `json:"podUid,omitempty"`
	Namespace string          `json:"namespace,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	At        time.Time       `json:"at,omitempty"`
}

// emptyResultJSON is the result payload of ops without an outcome value.
var emptyResultJSON = json.RawMessage("{}")

// Op-specific argument and result documents.
type pinImageArgs struct {
	Image string `json:"image"`
}

type pinImageResult struct {
	ManifestDigest string `json:"manifestDigest"`
}

type unpinImageArgs struct {
	Image string `json:"image"`
}

type leaseDevicesArgs struct {
	SandboxID      string `json:"sandboxId"`
	Image          string `json:"image"`
	MemSizeMiB     int    `json:"memSizeMiB"`
	RootfsWritable bool   `json:"rootfsWritable"`
}

type releaseDevicesArgs struct {
	LeaseID string `json:"leaseId"`
}

// journal is the append-only JSON line log.
type journal struct {
	path string
	file *os.File
}

// openJournal creates the journal directory and opens the log for appending.
func openJournal(path string) (*journal, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("prepare agent journal directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open agent journal: %w", err)
	}
	return &journal{path: path, file: file}, nil
}

// append writes one line and flushes it to disk before returning.
func (j *journal) append(entry journalEntry) error {
	payload, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if _, err := j.file.Write(payload); err != nil {
		return err
	}
	return j.file.Sync()
}

// replay reads the log and visits every complete line in order. A trailing
// line without a newline (a crash mid-append) is reported so the caller can
// truncate it; a malformed line elsewhere is a genuine corruption and fails
// the replay.
func (j *journal) replay(visit func(journalEntry) error) (truncateLength int64, err error) {
	payload, err := os.ReadFile(j.path)
	if err != nil {
		return 0, err
	}
	complete := payload
	if len(payload) > 0 && payload[len(payload)-1] != '\n' {
		// A final segment without a newline: a partial write from a
		// crash. It is dropped and the log is truncated below.
		truncateLength = int64(bytes.LastIndexByte(payload, '\n') + 1)
		complete = payload[:truncateLength]
	}
	for _, line := range bytes.Split(complete, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var entry journalEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return 0, fmt.Errorf("agent journal %s: corrupt line: %w", j.path, err)
		}
		if err := visit(entry); err != nil {
			return 0, err
		}
	}
	return truncateLength, nil
}

// truncate resizes the log to length, dropping a trailing partial line.
func (j *journal) truncate(length int64) error {
	if length <= 0 {
		length = 0
	}
	return os.Truncate(j.path, length)
}

func (j *journal) close() error {
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}
