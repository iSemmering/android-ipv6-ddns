package app

import (
	"context"
	"os"
	"path/filepath"
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
