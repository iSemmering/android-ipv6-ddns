package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLogElapsedAndLegacyEntries(t *testing.T) {
	for elapsed, want := range map[time.Duration]string{
		455 * time.Millisecond:   "455ms",
		1213 * time.Millisecond:  "1s 213ms",
		63021 * time.Millisecond: "1m 3s 21ms",
	} {
		if got := formatElapsed(elapsed); got != want {
			t.Errorf("duration for %v = %q, want %q", elapsed, got, want)
		}
	}
	path := filepath.Join(t.TempDir(), "legacy.jsonl")
	if err := os.WriteFile(path, []byte(`{"outcome":"失败","duration":"1.21 秒"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := readEntries(path)
	if err != nil || len(rows) != 1 || rows[0].Duration != "1s 210ms" || rows[0].Severity != "error" {
		t.Fatalf("legacy log was not normalized: %+v, %v", rows, err)
	}
}

func TestLogPageFiltersBeforePaginating(t *testing.T) {
	dir := t.TempDir()
	hour := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	var data []byte
	for i := 0; i < 204; i++ {
		row := Entry{ID: hour.UnixMicro() + int64(i+1), Outcome: "成功", Severity: "success"}
		if i%2 == 0 {
			row.Outcome, row.Severity = "等待", "pending"
		}
		if i == 0 {
			row.Severity = "" // Entries written by older versions have no severity.
		}
		line, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		data = append(append(data, line...), '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, hour.Format(logHour)+".jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	logs := &logStore{dir: dir, loc: time.UTC}
	first, cursor, err := logs.page(3, "warning", "")
	if err != nil || len(first) != 100 || cursor != strconv.FormatInt(hour.UnixMicro()+5, 10) {
		t.Fatalf("first page: %d entries, cursor %q, error %v", len(first), cursor, err)
	}
	for i, row := range first {
		if row.Severity != "pending" || row.ID != hour.UnixMicro()+int64(203-2*i) {
			t.Fatalf("entry %d: %+v", i, row)
		}
	}
	last, next, err := logs.page(3, "warning", cursor)
	if err != nil || len(last) != 2 || next != "" || last[1].ID != hour.UnixMicro()+1 || last[1].Severity != "pending" {
		t.Fatalf("last page: %+v, cursor %q, error %v", last, next, err)
	}
	all, _, err := logs.page(3, "", "")
	if err != nil || len(all) != 100 || all[0].Severity != "success" || all[1].Severity != "pending" {
		t.Fatalf("unfiltered page: %d entries, error %v", len(all), err)
	}
	if err := logs.append(3, "检查 IPv6", "失败", "测试", time.Now()); err != nil {
		t.Fatal(err)
	}
	heads := logs.heads()
	if heads["error"] == 0 || heads["all"] != heads["error"] || heads["info"] != 0 || heads["warning"] != 0 {
		t.Fatalf("unexpected log heads: %+v", heads)
	}
}

func TestModuleCardStatusPreservesPackageMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "module.prop")
	const metadata = "id=ipv6_ddns\nversion=1.0.0\nversionCode=100\n"
	if err := os.WriteFile(path, []byte(metadata+"description=initial\nauthor=Semmering\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e, err := New(dir, testNetwork{}, &testDNS{}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	wantDescription := func(want string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil || string(data) != metadata+"description="+want+" · 华为云 AAAA\nauthor=Semmering\n" {
			t.Fatalf("unexpected module description: %q, %v", data, err)
		}
	}
	e.SetModuleCard(path)
	t.Cleanup(e.stopModuleCard)
	wantDescription("⚪ 已停止")
	ctx := context.Background()
	config := e.configSnapshot()
	config.Enabled, config.Key.AK, config.Key.SK = true, "test-ak", "test-sk"
	config.Zone, config.Zones, config.Prefix = testZone, append(config.Zones, testZone), "phone"
	if err := e.saveLocked(config); err != nil {
		t.Fatal(err)
	}
	e.transition(ctx, "running", "运行中", "")
	wantDescription("🟢 运行中")
	e.transition(ctx, "working", "检查中", "")
	wantDescription("🟢 运行中")
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		data, err := os.ReadFile(path)
		if err != nil || strings.Contains(string(data), "🟡 检查中") {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	wantDescription("🟡 检查中")
	e.transition(ctx, "error", "同步失败", "")
	wantDescription("🔴 同步失败")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("module metadata permissions changed: %v, %v", info, err)
	}
}
