package app

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ipv6-ddns/internal/dns"
)

type Config struct {
	Enabled      bool             `json:"enabled"`
	Interval     int              `json:"interval"`
	KeepDataOn   bool             `json:"keepDataOn"`
	Airplane     bool             `json:"airplane"`
	Logging      bool             `json:"logging"`
	Days         int              `json:"days"`
	TTL          int              `json:"ttl"`
	Prefix       string           `json:"prefix"`
	Zone         dns.Zone         `json:"zone"`
	Zones        []dns.Zone       `json:"zones"`
	Key          dns.Credentials  `json:"credentials"`
	Draft        *dns.Credentials `json:"credentialDraft,omitempty"`
	RecordID     string           `json:"recordId"`
	Pending      bool             `json:"pending"`
	PendingWrite bool             `json:"pendingWrite,omitempty"`
	Owner        string           `json:"owner"`
	Last         Sync             `json:"last"`
}

type Sync struct {
	Address string `json:"address"`
	TTL     int    `json:"ttl"`
	At      int64  `json:"at"`
}

func (c Config) credentialDraft() dns.Credentials {
	if c.Draft != nil {
		return *c.Draft
	}
	return c.Key
}

func (c Config) Name() string {
	if c.Zone.ID == "" || c.Prefix == "" {
		return ""
	}
	return c.Prefix + "." + strings.TrimSuffix(c.Zone.Name, ".")
}

func (c Config) Ready() bool {
	if c.Key.AK == "" || c.Key.SK == "" || c.Name() == "" {
		return false
	}
	for _, zone := range c.Zones {
		if zone == c.Zone {
			return true
		}
	}
	return false
}

type View struct {
	Enabled, Logging, KeepDataOn, Airplane bool
	Interval, Days, TTL                    int
	Prefix                                 string
	Zone                                   dns.Zone
	Zones                                  []dns.Zone
	CredentialsSaved                       bool
	Domain, LastSync                       string
	Address, Phase, Label                  string
	Reason                                 string
	Busy                                   bool
	ManualPending                          bool
	LogHead                                map[string]int64
}

func loadConfig(dir string) (Config, error) {
	cfg := Config{Interval: 60, TTL: 1, Logging: true, Days: 3}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, errors.New("模块配置文件无法读取")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return cfg, err
	}
	if cfg.Owner == "" {
		var id [12]byte
		if _, err := rand.Read(id[:]); err != nil {
			return cfg, err
		}
		cfg.Owner = "IPv6 DDNS / " + hex.EncodeToString(id[:])
		if err := writeJSON(filepath.Join(dir, "config.json"), cfg); err != nil {
			return cfg, err
		}
	}
	if !validDays(cfg.Days) || cfg.Interval < 2 || cfg.Interval > 2147483 || cfg.TTL < 1 || cfg.TTL > 2147483647 {
		return cfg, errors.New("模块配置中的检查间隔、TTL 或日志保留天数超出范围")
	}
	return cfg, nil
}

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeAtomic(path, data, 0600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if mode != 0600 {
		err = f.Chmod(mode)
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func validDays(days int) bool {
	return days == 1 || days == 3 || days == 7
}

const dateFormat = "2006-01-02 15:04:05"
const logHour = "2006010215"

type Entry struct {
	ID       int64  `json:"id"`
	Time     string `json:"time"`
	Action   string `json:"action"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason"`
	Duration string `json:"duration"`
	Severity string `json:"severity"`
}

func logSeverity(outcome string) string {
	switch outcome {
	case "成功", "跳过":
		return "success"
	case "失败", "未恢复", "地址缺失":
		return "error"
	default:
		return "pending"
	}
}

func formatElapsed(elapsed time.Duration) string {
	ms := max(int64(0), elapsed.Milliseconds())
	if ms >= 60000 {
		return fmt.Sprintf("%dm %ds %dms", ms/60000, ms/1000%60, ms%1000)
	}
	if ms >= 1000 {
		return fmt.Sprintf("%ds %dms", ms/1000, ms%1000)
	}
	return fmt.Sprintf("%dms", ms)
}

type logStore struct {
	dir               string
	loc               *time.Location
	mu                sync.Mutex
	lastPrune, lastID int64
	lastInfo          atomic.Int64
	lastWarning       atomic.Int64
	lastError         atomic.Int64
}

func (s *logStore) heads() map[string]int64 {
	info, warning, failure := s.lastInfo.Load(), s.lastWarning.Load(), s.lastError.Load()
	return map[string]int64{"all": max(info, warning, failure), "info": info, "warning": warning, "error": failure}
}

func (s *logStore) append(days int, action, outcome, reason string, started time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	hour := now.Unix() / 3600
	if s.lastPrune != hour {
		if err := s.pruneUnlocked(days, now); err != nil {
			return err
		}
		s.lastPrune = hour
	}
	id := max(now.UnixMicro(), s.lastID+1)
	s.lastID = id
	entry := Entry{ID: id, Time: now.In(s.loc).Format(dateFormat), Action: action, Outcome: outcome,
		Reason: reason, Duration: formatElapsed(now.Sub(started)), Severity: logSeverity(outcome)}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, now.UTC().Format(logHour)+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	switch entry.Severity {
	case "success":
		s.lastInfo.Store(id)
	case "error":
		s.lastError.Store(id)
	default:
		s.lastWarning.Store(id)
	}
	return nil
}

func (s *logStore) prune(days int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneUnlocked(days, time.Now())
}

func (s *logStore) pruneUnlocked(days int, now time.Time) error {
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	files, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		hour, err := time.Parse(logHour, strings.TrimSuffix(f.Name(), ".jsonl"))
		if err != nil || f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(s.dir, f.Name())
		if !hour.Add(time.Hour).After(cutoff) {
			if err := os.Remove(path); err != nil {
				return err
			}
		} else if hour.Before(cutoff) {
			rows, err := readEntries(path)
			if err != nil {
				return err
			}
			var keep []byte
			for _, row := range rows {
				if row.ID >= cutoff.UnixMicro() {
					line, _ := json.Marshal(row)
					keep = append(append(keep, line...), '\n')
				}
			}
			if err := writeAtomic(path, keep, 0600); err != nil {
				return err
			}
		}
	}
	return nil
}

func readEntries(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows []Entry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var row Entry
		if json.Unmarshal(scanner.Bytes(), &row) == nil {
			if row.Severity == "" {
				row.Severity = logSeverity(row.Outcome)
			}
			if seconds, ok := strings.CutSuffix(row.Duration, " 秒"); ok {
				if elapsed, err := time.ParseDuration(seconds + "s"); err == nil {
					row.Duration = formatElapsed(elapsed)
				}
			}
			rows = append(rows, row)
		}
	}
	return rows, scanner.Err()
}

func (s *logStore) page(days int, level, cursor string) ([]Entry, string, error) {
	var severity string
	switch level {
	case "", "all":
	case "info":
		severity = "success"
	case "warning":
		severity = "pending"
	case "error":
		severity = "error"
	default:
		return nil, "", errors.New("日志类型无效")
	}
	before, err := strconv.ParseInt(cursor, 10, 64)
	if cursor != "" && err != nil {
		return nil, "", errors.New("日志分页参数无效")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	minID := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMicro()
	files, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, "", err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() > files[j].Name() })
	rows := []Entry{}
	for _, f := range files {
		hour, err := time.Parse(logHour, strings.TrimSuffix(f.Name(), ".jsonl"))
		if err != nil || f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") ||
			hour.Add(time.Hour).UnixMicro() <= minID || (before > 0 && hour.UnixMicro() > before) {
			continue
		}
		entries, err := readEntries(filepath.Join(s.dir, f.Name()))
		if err != nil {
			return nil, "", err
		}
		for i := len(entries) - 1; i >= 0; i-- {
			row := entries[i]
			if row.ID >= minID && (before == 0 || row.ID < before) && (severity == "" || row.Severity == severity) {
				if len(rows) == 100 {
					return rows, strconv.FormatInt(rows[len(rows)-1].ID, 10), nil
				}
				rows = append(rows, row)
			}
		}
	}
	return rows, "", nil
}
