package tui

// Log-tail seam (plan Task 5): the dashboard's log viewport is fed by an
// injected LogSource so tests never touch the real filesystem, while
// production reads the daemon's XDG file log (the same path fileLog() in
// cmd/zen-router/main.go tees to). Reading happens only inside the fetch
// command (Update/Init path) — never in View().

import (
	"io"
	"os"
	"strings"
)

const (
	// logTailLines is how many trailing lines one fetch cycle returns.
	// Large enough for the logs tab's scrollback + level/text filters
	// (Phase 2) — the whole tail is kept client-side.
	logTailLines = 5000
	// logTailBytes bounds how much of the file tail is read per cycle.
	logTailBytes = 1 << 20
)

// NewFileLogTail returns a LogSource for the daemon log file at path.
// A missing file yields an empty tail (no error): the daemon may simply
// not have logged yet.
func NewFileLogTail(path string) LogSource {
	return fileLogTail{path: path}
}

type fileLogTail struct {
	path string
}

// Tail returns at most maxLines trailing lines of the log file.
func (f fileLogTail) Tail(maxLines int) ([]string, error) {
	file, err := os.Open(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	st, err := file.Stat()
	if err != nil {
		return nil, err
	}
	var start int64
	if st.Size() > logTailBytes {
		start = st.Size() - logTailBytes
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	if start > 0 {
		// A window cut into the middle of a line: drop the partial first
		// line (unless it is the only one, which cannot happen when
		// start > 0 and the file ends in a full line).
		lines = lines[1:]
	}
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines, nil
}
