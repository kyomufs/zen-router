// Package store persists request-outcome events in a SQLite database — the
// raw material for the TUI statistics views (per-day and per-egress-IP
// OK/429 counts). The daemon owns the database exclusively; every reader
// goes through the control API, so SQLite never crosses the TUI import
// boundary (spec: "daemon-centric, TUI stays a pure HTTP client").
//
// Design notes:
//   - modernc.org/sqlite: pure Go, no cgo, works on NixOS.
//   - WAL journal: the request path inserts while the control API reads.
//   - Raw API keys NEVER reach the database — only 8-hex-char fingerprints
//     (store.Fingerprint), mirroring the status-payload redaction.
//   - Recording is auxiliary: every failure returns an error that the
//     caller logs, it must never affect request handling.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // register the "sqlite" driver
)

// Kind values stored in requests.kind.
const (
	KindOK  = "ok"
	Kind429 = "429"
)

// DayStat is one aggregated calendar day: how many OK and 429 events were
// recorded (newest-first ordering is the query's job). JSON tags are the
// GET /_zenctl/stats wire format.
type DayStat struct {
	Day  string `json:"day"` // YYYY-MM-DD (local time at record)
	OK   int64  `json:"ok"`
	N429 int64  `json:"429"`
}

// IPStat is one aggregated egress IP over the query window.
type IPStat struct {
	IP     string `json:"ip"` // observed public IP, or "unknown"
	OK     int64  `json:"ok"`
	N429   int64  `json:"429"`
	LastTS int64  `json:"last"` // unix ms of the most recent event for this IP
}

// Store is a SQLite-backed request history. Safe for concurrent use.
type Store struct {
	db *sql.DB

	ipMu sync.RWMutex
	ip   func() string // egress IP getter, nil = "unknown"
}

// Fingerprint derives the stable display form of one raw API key: 8 hex
// chars of sha256 — enough to tell keys apart without disclosing any part
// of the secret (same derivation as the status payload).
func Fingerprint(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:4])
}

// Open opens (creating as needed) the history database at path and ensures
// the schema exists. The database runs in WAL mode so the request-path
// inserts never block control-API reads.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty database path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store: create db dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite accepts one writer; WAL lets readers proceed in parallel and
	// the busy timeout absorbs the rare write-write race instead of
	// surfacing SQLITE_BUSY to the request path.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: enable WAL: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: set busy_timeout: %w", err)
	}
	const schema = `
CREATE TABLE IF NOT EXISTS requests(
	id     INTEGER PRIMARY KEY,
	ts     INTEGER NOT NULL, -- unix ms at record time
	day    TEXT    NOT NULL, -- YYYY-MM-DD (local), for GROUP BY
	ip     TEXT    NOT NULL, -- observed egress IP or "unknown"
	key_fp TEXT    NOT NULL, -- 8-hex fingerprint, "" when no key (legacy path)
	kind   TEXT    NOT NULL  -- "ok" | "429"
);
CREATE INDEX IF NOT EXISTS idx_requests_day ON requests(day);
CREATE INDEX IF NOT EXISTS idx_requests_ip ON requests(ip);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: create schema: %w", err)
	}
	return &Store{db: db}, nil
}

// SetIPGetter installs the egress IP observation used to stamp every
// subsequent event. The getter must be non-blocking (it is called on the
// request path); nil resets stamps to "unknown". Safe to call while the
// store serves traffic.
func (s *Store) SetIPGetter(get func() string) {
	if s == nil {
		return
	}
	s.ipMu.Lock()
	s.ip = get
	s.ipMu.Unlock()
}

// Record appends one outcome event: kind is KindOK or Kind429, rawKey the
// API key the attempt used ("" on the legacy proxy path, where no key is
// known — stored as ""). The timestamp, local day, fingerprint and egress
// IP are stamped here so callers never assemble history rows themselves.
func (s *Store) Record(kind, rawKey string) error {
	if s == nil {
		return nil
	}
	now := time.Now()
	_, err := s.db.Exec(
		`INSERT INTO requests(ts, day, ip, key_fp, kind) VALUES(?, ?, ?, ?, ?)`,
		now.UnixMilli(), now.Format("2006-01-02"), s.currentIP(), Fingerprint(rawKey), kind,
	)
	if err != nil {
		return fmt.Errorf("store: record %s: %w", kind, err)
	}
	return nil
}

// currentIP reads the installed getter under lock; no getter (or an empty
// observation) stamps "unknown".
func (s *Store) currentIP() string {
	s.ipMu.RLock()
	get := s.ip
	s.ipMu.RUnlock()
	if get == nil {
		return "unknown"
	}
	if ip := get(); ip != "" {
		return ip
	}
	return "unknown"
}

// ByDay aggregates OK/429 counts per local calendar day for the last
// `days` days (inclusive of today), newest day first.
func (s *Store) ByDay(days int) ([]DayStat, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.db.Query(`
SELECT day,
       SUM(CASE WHEN kind = 'ok'  THEN 1 ELSE 0 END),
       SUM(CASE WHEN kind = '429' THEN 1 ELSE 0 END)
  FROM requests
 WHERE day >= ?
 GROUP BY day
 ORDER BY day DESC`, cutoffDay(days))
	if err != nil {
		return nil, fmt.Errorf("store: by day: %w", err)
	}
	defer rows.Close()
	out := []DayStat{}
	for rows.Next() {
		var d DayStat
		if err := rows.Scan(&d.Day, &d.OK, &d.N429); err != nil {
			return nil, fmt.Errorf("store: by day scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ByIP aggregates OK/429 counts per observed egress IP for the last `days`
// days, most recently seen first.
func (s *Store) ByIP(days int) ([]IPStat, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.db.Query(`
SELECT ip,
       SUM(CASE WHEN kind = 'ok'  THEN 1 ELSE 0 END),
       SUM(CASE WHEN kind = '429' THEN 1 ELSE 0 END),
       MAX(ts)
  FROM requests
 WHERE day >= ?
 GROUP BY ip
 ORDER BY MAX(ts) DESC`, cutoffDay(days))
	if err != nil {
		return nil, fmt.Errorf("store: by ip: %w", err)
	}
	defer rows.Close()
	out := []IPStat{}
	for rows.Next() {
		var p IPStat
		if err := rows.Scan(&p.IP, &p.OK, &p.N429, &p.LastTS); err != nil {
			return nil, fmt.Errorf("store: by ip scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Close releases the database handle (nil-safe).
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	return s.db.Close()
}

// cutoffDay is the oldest local calendar day in a `days`-wide window
// ending today (inclusive): days=7 keeps today minus 6 days.
func cutoffDay(days int) string {
	if days < 1 {
		days = 1
	}
	return time.Now().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
}
