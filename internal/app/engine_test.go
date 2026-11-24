package app

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ipv6-ddns/internal/android"
	"ipv6-ddns/internal/dns"
)

type testNetwork struct{}

func (testNetwork) Inspect(context.Context, netip.Addr) (android.Snapshot, error) {
	return android.Snapshot{Interface: "data1", Address: netip.MustParseAddr("2409::2"), Reason: "蜂窝地址有效"}, nil
}
func (testNetwork) Radio(context.Context) android.Radio {
	return android.Radio{DataKnown: true, DataOn: true, AirplaneKnown: true}
}
func (testNetwork) SetData(context.Context, bool) error     { panic("unexpected data toggle") }
func (testNetwork) SetAirplane(context.Context, bool) error { panic("unexpected airplane toggle") }

type testDNS struct {
	record             dns.Record
	updates            int
	reads              int
	zoneErr, recordErr error
}

var testZone = dns.Zone{ID: "zone1", Name: "example.com"}

func (f *testDNS) Zones(context.Context, dns.Credentials) ([]dns.Zone, error) {
	return []dns.Zone{testZone}, f.zoneErr
}
func (f *testDNS) FindAAAA(context.Context, dns.Credentials, dns.Zone, string) ([]dns.Record, error) {
	return []dns.Record{f.record}, nil
}
func (f *testDNS) Record(context.Context, dns.Credentials, dns.Zone, string) (dns.Record, error) {
	f.reads++
	return f.record, f.recordErr
}
func (*testDNS) CreateAAAA(context.Context, dns.Credentials, dns.Zone, string, netip.Addr, int, string) (dns.Record, error) {
	panic("unexpected record creation")
}
func (f *testDNS) UpdateAAAA(_ context.Context, _ dns.Credentials, _ dns.Zone, _ dns.Record, addr netip.Addr, ttl int) (dns.Record, error) {
	f.updates++
	f.record.Values = []string{addr.String()}
	f.record.TTL = ttl
	f.record.Status = "PENDING_UPDATE"
	return f.record, nil
}

func TestConfirmationAndPersistentSync(t *testing.T) {
	f := &testDNS{record: dns.Record{ID: "existing", Name: "phone.example.com.", Type: "AAAA", Values: []string{"2409::1"}, TTL: 300, Status: "ACTIVE"}}
	dir := t.TempDir()
	e, err := New(dir, testNetwork{}, f, time.FixedZone("test", 28800))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	on := true
	if e.status().KeepDataOn {
		t.Fatal("automatically reopening mobile data should require opt-in")
	}
	if e.Call(ctx, Request{Op: "toggle", Enabled: &on}).OK {
		t.Fatal("cannot enable before configuring a valid binding")
	}
	if r := e.Call(ctx, Request{Op: "verify", AK: "testAK", SK: "testSK"}); !r.OK {
		t.Fatal(r.Error)
	}
	save := Request{Op: "save", ZoneID: testZone.ID, Prefix: "phone", TTL: 1, Interval: 30, KeepDataOn: &on}
	r := e.Call(ctx, save)
	if r.Confirm == nil || r.Confirm.ID != "existing" || f.updates != 0 {
		t.Fatal("must ask before taking control of an existing record")
	}
	save.Confirm = r.Confirm.ID
	if r := e.Call(ctx, save); !r.OK || r.Confirm != nil {
		t.Fatalf("save: %+v", r)
	}
	if r := e.Call(ctx, Request{Op: "toggle", Enabled: &on}); !r.OK {
		t.Fatal(r.Error)
	}
	ip := netip.MustParseAddr("2409::2")
	e.check(ctx, e.configSnapshot(), ip, false)
	if f.updates != 1 || !e.configSnapshot().Pending || e.configSnapshot().Last.At != 0 {
		t.Fatal("an HTTP acceptance must not count as completed synchronization")
	}
	f.record.Status = "ACTIVE"
	e.check(ctx, e.configSnapshot(), ip, true)
	if f.updates != 1 || e.configSnapshot().Last.At == 0 || e.configSnapshot().Last.TTL != 1 {
		t.Fatal("confirmation should record the effective TTL without writing twice")
	}
	confirmed := e.configSnapshot()
	confirmed.Last.At -= 60
	if err := e.saveLocked(confirmed); err != nil {
		t.Fatal(err)
	}
	reads := f.reads
	e.check(ctx, confirmed, ip, true)
	if f.reads != reads+1 || f.updates != 1 || e.configSnapshot().Last != confirmed.Last {
		t.Fatal("manual check must verify the cloud record without updating a match or its last write time")
	}
	logs := e.Call(ctx, Request{Op: "logs"})
	if len(logs.Entries) == 0 || logs.Entries[0].Outcome != "跳过" || logs.Entries[0].Severity != "success" {
		t.Fatal("matching records should be logged as skipped")
	}
	e.check(ctx, e.configSnapshot(), ip, false)
	if f.reads != reads+1 || f.updates != 1 {
		t.Fatal("automatic checks should reuse the last confirmed match")
	}
	reopened, err := New(dir, testNetwork{}, f, time.UTC)
	if err != nil || !reopened.status().Enabled || !reopened.status().KeepDataOn || reopened.status().Interval != 30 {
		t.Fatal("automatic sync, mobile data preference, and interval must survive daemon restart", err)
	}
	off := false
	save.KeepDataOn = nil
	if r := reopened.Call(ctx, save); !r.OK || !r.Status.KeepDataOn {
		t.Fatal("older clients should leave the existing mobile data preference intact")
	}
	save.KeepDataOn = &off
	if r := reopened.Call(ctx, save); !r.OK || r.Status.KeepDataOn {
		t.Fatal("explicitly disabling the mobile data preference should take effect")
	}
	reopened.Call(ctx, Request{Op: "toggle", Enabled: &off})
	cfg, err := loadConfig(dir)
	if err != nil || cfg.Enabled || cfg.KeepDataOn {
		t.Fatal("the disabled switches must be persisted")
	}
	data, _ := os.Stat(filepath.Join(dir, "config.json"))
	if data.Mode().Perm() != 0600 {
		t.Fatal("credentials must be in a root-only file")
	}
}

type mobileDataTestNetwork struct {
	testNetwork
	radio       android.Radio
	enableError error
	enableCalls int
	inspections int
}

func (n *mobileDataTestNetwork) Radio(context.Context) android.Radio { return n.radio }

func (n *mobileDataTestNetwork) SetData(_ context.Context, enabled bool) error {
	if !enabled {
		panic("a health check should only turn data on in this test")
	}
	n.enableCalls++
	if n.enableError == nil {
		n.radio.DataOn = true
	}
	return n.enableError
}

func (n *mobileDataTestNetwork) Inspect(ctx context.Context, previous netip.Addr) (android.Snapshot, error) {
	n.inspections++
	if !n.radio.DataOn {
		return android.Snapshot{}, errors.New("测试移动数据关闭")
	}
	return n.testNetwork.Inspect(ctx, previous)
}

func TestCheckKeepsMobileDataOn(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		keepDataOn, dataOn, airplaneOn bool
		enableError                    error
		enableCalls, inspections       int
		phase                          string
	}{
		{name: "opted in", keepDataOn: true, enableCalls: 1, inspections: 1, phase: "running"},
		{name: "already enabled", keepDataOn: true, dataOn: true, inspections: 1, phase: "running"},
		{name: "opted out", phase: "waiting"},
		{name: "airplane mode", keepDataOn: true, airplaneOn: true, phase: "waiting"},
		{name: "failed to enable", keepDataOn: true, enableError: errors.New("测试开启命令失败"), enableCalls: 1, phase: "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &mobileDataTestNetwork{
				radio:       android.Radio{DataKnown: true, DataOn: tc.dataOn, AirplaneKnown: true, AirplaneOn: tc.airplaneOn},
				enableError: tc.enableError,
			}
			f := &testDNS{record: dns.Record{
				ID: "existing", Name: "phone.example.com.", Type: "AAAA", Values: []string{"2409::2"}, TTL: 1, Status: "ACTIVE",
			}}
			e, err := New(t.TempDir(), n, f, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			cfg := e.configSnapshot()
			cfg.Enabled, cfg.KeepDataOn = true, tc.keepDataOn
			cfg.Key = dns.Credentials{AK: "test-ak", SK: "test-sk"}
			cfg.Zone, cfg.Zones = testZone, []dns.Zone{testZone}
			cfg.Prefix, cfg.RecordID = "phone", "existing"
			if err := e.saveLocked(cfg); err != nil {
				t.Fatal(err)
			}
			e.check(context.Background(), cfg, netip.Addr{}, true)
			if n.enableCalls != tc.enableCalls || n.inspections != tc.inspections || f.reads != tc.inspections || e.status().Phase != tc.phase {
				t.Fatalf("enable calls = %d, IPv6 reads = %d, DNS reads = %d, phase = %q",
					n.enableCalls, n.inspections, f.reads, e.status().Phase)
			}
		})
	}
}

func TestCredentialDraftSurvivesFailedValidation(t *testing.T) {
	dir := t.TempDir()
	f := &testDNS{}
	e, err := New(dir, testNetwork{}, f, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	verified := dns.Credentials{AK: "verified-ak", SK: "verified-sk"}
	c := e.configSnapshot()
	c.Key = verified
	if err := e.saveLocked(c); err != nil {
		t.Fatal(err)
	}
	if r := e.Call(ctx, Request{Op: "getDraft"}); r.Draft == nil || *r.Draft != verified {
		t.Fatal("keys from an earlier version should prefill the form")
	}
	f.zoneErr = errors.New("测试凭据无权访问")
	if r := e.Call(ctx, Request{Op: "verify", AK: " revised-ak ", SK: "revised-sk", Revision: 20}); r.OK || r.Error != f.zoneErr.Error() {
		t.Fatal("should report provider rejection")
	}
	blank, stale := "", "stale-sk"
	for _, r := range []Request{
		{Op: "saveDraft", Draft: &DraftEdit{SK: &blank}, Revision: 30},
		{Op: "saveDraft", Draft: &DraftEdit{SK: &stale}, Revision: 29},
	} {
		if reply := e.Call(ctx, r); !reply.OK {
			t.Fatal(reply.Error)
		}
	}
	reopened, err := New(dir, testNetwork{}, f, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	r := reopened.Call(ctx, Request{Op: "getDraft"})
	if r.Draft == nil || r.Draft.AK != " revised-ak " || r.Draft.SK != "" || reopened.configSnapshot().Key != verified {
		t.Fatal("draft edits and validated credentials must survive restart independently")
	}
}

type inspection struct {
	ctx     context.Context
	release chan struct{}
}

type interruptibleNetwork struct {
	testNetwork
	started chan inspection
}

func (n interruptibleNetwork) Inspect(ctx context.Context, previous netip.Addr) (android.Snapshot, error) {
	call := inspection{ctx: ctx, release: make(chan struct{})}
	select {
	case n.started <- call:
	case <-ctx.Done():
		return android.Snapshot{}, ctx.Err()
	}
	select {
	case <-call.release:
		return n.testNetwork.Inspect(ctx, previous)
	case <-ctx.Done():
		return android.Snapshot{}, ctx.Err()
	}
}

func TestManualCheckPreemptsAndRestartsInterval(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "cloud match"
		if fail {
			name = "cloud error"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testManualCheckPreemption(t, fail)
		})
	}
}

func testManualCheckPreemption(t *testing.T, fail bool) {
	network := interruptibleNetwork{started: make(chan inspection)}
	f := &testDNS{record: dns.Record{ID: "existing", Name: "phone.example.com.", Type: "AAAA", Values: []string{"2409::2"}, TTL: 1, Status: "ACTIVE"}}
	if fail {
		f.recordErr = errors.New("测试云端失败")
	}
	e, err := New(t.TempDir(), network, f, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	config := e.configSnapshot()
	config.Enabled, config.Interval, config.TTL = true, 2, 1
	config.Key = dns.Credentials{AK: "test-ak", SK: "test-sk"}
	config.Zone, config.Zones = testZone, []dns.Zone{testZone}
	config.Prefix, config.RecordID = "phone", "existing"
	if err := e.saveLocked(config); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	next := func(timeout time.Duration) inspection {
		t.Helper()
		select {
		case call := <-network.started:
			return call
		case <-time.After(timeout):
			t.Fatal("expected an IPv6 check to start")
			return inspection{}
		}
	}
	automatic := next(time.Second)
	if reply := e.Call(ctx, Request{Op: "check"}); !reply.OK || !reply.Status.ManualPending {
		t.Fatal("manual check was not accepted")
	}
	manual := next(time.Second)
	if !errors.Is(context.Cause(automatic.ctx), errManualCheck) {
		t.Fatal("the running automatic check should be interrupted")
	}
	if reply := e.Call(ctx, Request{Op: "check"}); !reply.OK || !reply.Status.ManualPending {
		t.Fatal("a second request during a manual check should share the current check")
	}
	finishedAfter := time.Now()
	close(manual.release)
	following := next(4 * time.Second)
	if time.Since(finishedAfter) < 2*time.Second || e.status().ManualPending || following.ctx.Err() != nil {
		t.Fatal("automatic scheduling must restart from the end of the manual check")
	}
	if f.reads != 1 || f.updates != 0 {
		t.Fatal("manual check should perform one cloud read and avoid any duplicate write")
	}
	if fail {
		close(following.release)
		deadline := time.Now().Add(time.Second)
		for e.status().Busy && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if e.status().Busy || f.reads != 2 {
			t.Fatal("the next automatic check must perform its cloud verification after a manual failure")
		}
	}
}
