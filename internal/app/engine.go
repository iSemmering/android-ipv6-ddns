package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"ipv6-ddns/internal/android"
	"ipv6-ddns/internal/dns"
)

type Network interface {
	Inspect(context.Context, netip.Addr) (android.Snapshot, error)
	Radio(context.Context) android.Radio
	SetData(context.Context, bool) error
	SetAirplane(context.Context, bool) error
}

type Request struct {
	Op         string     `json:"op"`
	AK         string     `json:"ak,omitempty"`
	SK         string     `json:"sk,omitempty"`
	ZoneID     string     `json:"zoneId,omitempty"`
	Prefix     string     `json:"prefix,omitempty"`
	Confirm    string     `json:"confirm,omitempty"`
	TTL        int        `json:"ttl,omitempty"`
	Interval   int        `json:"interval,omitempty"`
	KeepDataOn *bool      `json:"keepDataOn,omitempty"`
	Airplane   bool       `json:"airplane,omitempty"`
	Enabled    *bool      `json:"enabled,omitempty"`
	Logging    *bool      `json:"logging,omitempty"`
	Days       int        `json:"days,omitempty"`
	Level      string     `json:"level,omitempty"`
	Cursor     string     `json:"cursor,omitempty"`
	Draft      *DraftEdit `json:"draft,omitempty"`
	Revision   int64      `json:"revision,omitempty"`
}

type DraftEdit struct {
	AK *string `json:"ak,omitempty"`
	SK *string `json:"sk,omitempty"`
}

type Confirmation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Reply struct {
	OK      bool             `json:"ok"`
	Error   string           `json:"error,omitempty"`
	Status  *View            `json:"status,omitempty"`
	Confirm *Confirmation    `json:"confirm,omitempty"`
	Entries []Entry          `json:"entries,omitempty"`
	Cursor  string           `json:"cursor,omitempty"`
	Draft   *dns.Credentials `json:"draft,omitempty"`
}

type Engine struct {
	dir                              string
	network                          Network
	provider                         dns.Provider
	logs                             logStore
	card                             moduleCard
	apiMu                            sync.Mutex
	mu                               sync.Mutex
	config                           Config
	address                          netip.Addr
	phase, label, reason             string
	busy, conflict                   bool
	manualRequested, manualCompleted uint64
	draftAKRevision, draftSKRevision int64
	cancel                           context.CancelCauseFunc
	wake                             chan struct{}
	lastRecovery                     time.Time
	lastPassive                      time.Time
	retryAt                          time.Time
	cloudError                       string
	cloudFails                       int
}

func New(dir string, network Network, provider dns.Provider, tz *time.Location) (*Engine, error) {
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return nil, err
	}
	config, err := loadConfig(dir)
	if err != nil {
		return nil, err
	}
	e := &Engine{dir: dir, network: network, provider: provider, config: config,
		wake: make(chan struct{}, 1), logs: logStore{dir: logDir, loc: tz}}
	if err := e.logs.prune(config.Days); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) configSnapshot() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.config
}

func (e *Engine) status() *View {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.config
	phase, label, reason := e.displayStateLocked()
	report := &View{
		Enabled: c.Enabled, Logging: c.Logging, KeepDataOn: c.KeepDataOn, Airplane: c.Airplane,
		Interval: c.Interval, Days: c.Days, TTL: c.TTL, Prefix: c.Prefix, Zone: c.Zone,
		Zones: append([]dns.Zone{}, c.Zones...), CredentialsSaved: c.Key.AK != "" && c.Key.SK != "",
		Domain: c.Name(), Phase: phase, Label: label, Reason: reason,
		LogHead:       e.logs.heads(),
		Busy:          e.busy && c.Enabled && c.Ready(),
		ManualPending: c.Enabled && c.Ready() && e.manualRequested > e.manualCompleted,
	}
	if e.address.IsValid() {
		report.Address = e.address.String()
	}
	if c.Last.At != 0 {
		report.LastSync = time.Unix(c.Last.At, 0).In(e.logs.loc).Format(dateFormat)
	}
	return report
}

func (e *Engine) displayStateLocked() (phase, label, reason string) {
	switch {
	case !e.config.Enabled:
		return "stopped", "已停止", ""
	case !e.config.Ready():
		return "error", "等待配置", "请确认密钥及选择要绑定的公网域名"
	case e.phase == "":
		return "working", "等待检查", ""
	default:
		return e.phase, e.label, e.reason
	}
}

func (e *Engine) record(action, outcome, reason string, started time.Time) {
	c := e.configSnapshot()
	if c.Logging {
		reason = strings.NewReplacer("context canceled", "操作已取消",
			"context deadline exceeded", "等待超时", "\n", " · ").Replace(reason)
		if err := e.logs.append(c.Days, action, outcome, reason, started); err != nil {
			fmt.Fprintln(os.Stderr, "写入模块运行日志失败")
		}
	}
}

func (e *Engine) transition(ctx context.Context, phase, label, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ctx.Err() == nil {
		e.phase, e.label, e.reason = phase, label, reason
		e.updateModuleCardLocked()
	}
}

func (e *Engine) updateAddress(ip netip.Addr) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.address = ip
}

func (e *Engine) saveLocked(c Config) error {
	if err := writeJSON(filepath.Join(e.dir, "config.json"), c); err != nil {
		return errors.New("无法保存模块配置")
	}
	e.config = c
	return nil
}

func (e *Engine) rescheduleLocked() {
	if e.cancel != nil {
		e.cancel(nil)
	}
	e.phase, e.label, e.reason = "", "", ""
	if !e.config.Enabled || !e.config.Ready() {
		e.manualCompleted = e.manualRequested
	}
	e.retryAt, e.cloudError, e.cloudFails = time.Time{}, "", 0
	e.updateModuleCardLocked()
	e.queue()
}

func (e *Engine) queue() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9._+=/-]{1,128}$`)
var prefixPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var domainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
var errManualCheck = errors.New("收到手动检查请求")

func (e *Engine) saveDraft(edit DraftEdit, revision int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.config
	draft := c.credentialDraft()
	akEdited := edit.AK != nil && (revision == 0 || revision >= e.draftAKRevision)
	skEdited := edit.SK != nil && (revision == 0 || revision >= e.draftSKRevision)
	if akEdited {
		draft.AK = *edit.AK
	}
	if skEdited {
		draft.SK = *edit.SK
	}
	if (akEdited || skEdited) && (c.Draft == nil || draft != *c.Draft) {
		c.Draft = &draft
		if err := e.saveLocked(c); err != nil {
			return errors.New("无法保存访问密钥草稿")
		}
	}
	if akEdited {
		e.draftAKRevision = max(e.draftAKRevision, revision)
	}
	if skEdited {
		e.draftSKRevision = max(e.draftSKRevision, revision)
	}
	return nil
}

func (e *Engine) verify(ctx context.Context, request Request) error {
	if err := e.saveDraft(DraftEdit{AK: &request.AK, SK: &request.SK}, request.Revision); err != nil {
		return err
	}
	key := dns.Credentials{AK: strings.TrimSpace(request.AK), SK: strings.TrimSpace(request.SK)}
	if !keyPattern.MatchString(key.AK) || !keyPattern.MatchString(key.SK) {
		return errors.New("请输入有效的 AK 和 SK")
	}
	e.apiMu.Lock()
	defer e.apiMu.Unlock()
	start := time.Now()
	zones, err := e.provider.Zones(ctx, key)
	if err != nil {
		e.record("获取公网域名", "失败", err.Error(), start)
		return err
	}
	if len(zones) == 0 {
		reason := "未找到处于正常状态且当前密钥有权访问的华为云公网域名"
		e.record("获取公网域名", "失败", reason, start)
		return errors.New(reason)
	}
	e.mu.Lock()
	c := e.config
	changed := c.Key != key
	c.Key, c.Zones = key, zones
	err = e.saveLocked(c)
	if err == nil && (changed || !c.Ready()) {
		e.rescheduleLocked()
	}
	e.mu.Unlock()
	if err == nil {
		e.record("获取公网域名", "成功", fmt.Sprintf("华为云返回 %d 个可用公网域名", len(zones)), start)
	}
	return err
}

func (e *Engine) saveBinding(ctx context.Context, request Request) (*Confirmation, error) {
	prefix := strings.ToLower(strings.TrimSpace(request.Prefix))
	if !prefixPattern.MatchString(prefix) {
		return nil, errors.New("请输入有效的一级子域名前缀")
	}
	if request.TTL < 1 || request.TTL > 2147483647 || request.Interval < 2 || request.Interval > 2147483 {
		return nil, errors.New("TTL 须为 1 至 2147483647 秒，检查间隔须为 2 至 2147483 秒")
	}
	e.apiMu.Lock()
	defer e.apiMu.Unlock()
	e.mu.Lock()
	old, conflict := e.config, e.conflict
	e.mu.Unlock()
	zone := dns.Zone{}
	for _, candidate := range old.Zones {
		if candidate.ID == request.ZoneID {
			zone = candidate
			break
		}
	}
	if zone.ID == "" || !domainPattern.MatchString(zone.Name) || len(prefix+"."+zone.Name) > 253 {
		return nil, errors.New("请选择已验证的公网域名")
	}
	name := prefix + "." + zone.Name + "."
	changed := old.Zone != zone || old.Prefix != prefix
	recordID := old.RecordID
	start := time.Now()
	if changed || conflict {
		recordID = ""
		records, err := e.provider.FindAAAA(ctx, old.Key, zone, name)
		if err != nil {
			e.record("检查同名 AAAA", "失败", err.Error(), start)
			return nil, err
		}
		if len(records) > 1 || (len(records) == 1 && (records[0].Alias != nil || len(records[0].Values) > 1)) {
			return nil, errors.New("同名 AAAA 使用了多条记录、多个地址或别名，请选择独立的 DDNS 子域名")
		}
		if len(records) == 1 {
			record := records[0]
			if record.ID == "" {
				return nil, errors.New("华为云返回的 AAAA 记录缺少资源 ID")
			}
			if record.Description != old.Owner && request.Confirm != record.ID {
				return &Confirmation{ID: record.ID, Name: strings.TrimSuffix(record.Name, ".")}, nil
			}
			recordID = record.ID
		} else if request.Confirm != "" {
			return nil, errors.New("原 AAAA 记录已变化，请重新保存配置")
		}
	}
	e.mu.Lock()
	next := e.config
	next.Zone, next.Prefix, next.RecordID = zone, prefix, recordID
	next.TTL, next.Interval, next.Airplane = request.TTL, request.Interval, request.Airplane
	if request.KeepDataOn != nil {
		next.KeepDataOn = *request.KeepDataOn
	}
	if changed || next.RecordID != e.config.RecordID {
		next.Last = Sync{}
		next.Pending = false
		next.PendingWrite = false
	}
	err := e.saveLocked(next)
	if err == nil {
		e.conflict = false
		e.rescheduleLocked()
	}
	e.mu.Unlock()
	if err == nil {
		e.record("保存配置", "成功", fmt.Sprintf("%s · TTL %d 秒 · 检查间隔 %d 秒", next.Name(), next.TTL, next.Interval), start)
	}
	return nil, err
}

func (e *Engine) setEnabled(value bool) error {
	start := time.Now()
	e.mu.Lock()
	next := e.config
	if value && !next.Ready() {
		e.mu.Unlock()
		return errors.New("请先验证访问密钥并保存域名绑定")
	}
	next.Enabled = value
	err := e.saveLocked(next)
	if err == nil {
		e.rescheduleLocked()
	}
	e.mu.Unlock()
	if err == nil {
		reason := "已停止自动检查和同步"
		if value {
			reason = "已启用自动检查和同步"
		}
		e.record("自动同步开关", "成功", reason, start)
	}
	return err
}

func (e *Engine) setLogs(request Request) error {
	start := time.Now()
	e.mu.Lock()
	next := e.config
	if request.Logging != nil {
		next.Logging = *request.Logging
	}
	if request.Days != 0 {
		if !validDays(request.Days) {
			e.mu.Unlock()
			return errors.New("日志保留天数只能选择 1、3 或 7 天")
		}
		next.Days = request.Days
		if err := e.logs.prune(next.Days); err != nil {
			e.mu.Unlock()
			return errors.New("清理过期日志失败")
		}
	}
	err := e.saveLocked(next)
	e.mu.Unlock()
	if err == nil {
		e.record("日志设置", "成功", fmt.Sprintf("日志保留最近 %d 天", next.Days), start)
	}
	return err
}

func (e *Engine) Call(ctx context.Context, request Request) Reply {
	var result Reply
	var err error
	switch request.Op {
	case "status":
		e.mu.Lock()
		passive := !e.config.Enabled && time.Since(e.lastPassive) >= 15*time.Second
		previous := e.address
		if passive {
			e.lastPassive = time.Now()
		}
		e.mu.Unlock()
		if passive {
			if snapshot, inspectErr := e.network.Inspect(ctx, previous); inspectErr == nil {
				e.updateAddress(snapshot.Address)
			}
		}
	case "verify":
		err = e.verify(ctx, request)
	case "getDraft":
		draft := e.configSnapshot().credentialDraft()
		return Reply{OK: true, Draft: &draft}
	case "saveDraft":
		if request.Draft == nil {
			return Reply{Error: "缺少访问密钥草稿"}
		}
		if err := e.saveDraft(*request.Draft, request.Revision); err != nil {
			return Reply{Error: err.Error()}
		}
		return Reply{OK: true}
	case "save":
		result.Confirm, err = e.saveBinding(ctx, request)
	case "toggle":
		if request.Enabled == nil {
			err = errors.New("缺少自动同步开关值")
		} else {
			err = e.setEnabled(*request.Enabled)
		}
	case "logSettings":
		err = e.setLogs(request)
	case "logs":
		result.Entries, result.Cursor, err = e.logs.page(e.configSnapshot().Days, request.Level, request.Cursor)
	case "check":
		e.mu.Lock()
		if !e.config.Enabled || !e.config.Ready() {
			err = errors.New("请先开启自动同步")
		} else if e.manualRequested == e.manualCompleted {
			e.manualRequested++
			if e.cancel != nil {
				e.cancel(errManualCheck)
			}
			e.phase, e.label, e.reason = "working", "检查中", "正在启动手动检查"
			e.updateModuleCardLocked()
			e.queue()
		}
		e.mu.Unlock()
	default:
		err = errors.New("未知的模块操作")
	}
	if err != nil {
		return Reply{Error: err.Error()}
	}
	result.OK = true
	if request.Op != "logs" {
		result.Status = e.status()
	}
	return result
}

func (e *Engine) Run(ctx context.Context) {
	defer e.stopModuleCard()
	e.restorePending()
	for ctx.Err() == nil {
		started := time.Now()
		e.mu.Lock()
		// A queued wake represented by this snapshot need not run a second cycle.
		select {
		case <-e.wake:
		default:
		}
		c, previous := e.config, e.address
		runnable := c.Enabled && c.Ready()
		manual := e.manualRequested
		force := runnable && manual > e.manualCompleted
		check, cancel := context.WithCancelCause(ctx)
		e.cancel = cancel
		e.busy = runnable
		e.mu.Unlock()

		delay := time.Duration(c.Interval) * time.Second
		if force {
			e.record("手动检查", "开始", "立即检查蜂窝 INTERNET IPv6 与云端 AAAA", started)
		}
		if runnable {
			if retry := e.check(check, c, previous, force); retry > 0 && !force {
				delay = min(delay, retry)
			}
		}
		e.mu.Lock()
		if force && check.Err() == nil {
			e.manualCompleted = max(e.manualCompleted, manual)
			e.retryAt, e.cloudError, e.cloudFails = time.Time{}, "", 0
		}
		preempted := runnable && errors.Is(context.Cause(check), errManualCheck)
		cancel(nil)
		e.cancel, e.busy = nil, false
		e.mu.Unlock()
		if preempted {
			e.record("定时检查", "已取消", "已切换到手动检查", started)
		}

		if !runnable {
			select {
			case <-ctx.Done():
			case <-e.wake:
			}
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-e.wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (e *Engine) check(ctx context.Context, c Config, previous netip.Addr, force bool) time.Duration {
	start := time.Now()
	e.transition(ctx, "working", "检查中", "正在检查蜂窝 INTERNET 网络")
	radio := e.network.Radio(ctx)
	if ctx.Err() != nil {
		return 0
	}
	var snapshot android.Snapshot
	var err error
	switch {
	case radio.AirplaneKnown && radio.AirplaneOn:
		e.updateAddress(netip.Addr{})
		e.transition(ctx, "waiting", "等待移动数据", "系统飞行模式已开启")
		e.record("检查 IPv6", "暂停", "系统飞行模式已开启", start)
		return 0
	case radio.DataKnown && !radio.DataOn:
		e.updateAddress(netip.Addr{})
		if !c.KeepDataOn {
			e.transition(ctx, "waiting", "等待移动数据", "系统移动数据已关闭")
			e.record("检查 IPv6", "暂停", "系统移动数据已关闭", start)
			return 0
		}
		var ready bool
		snapshot, ready = e.enableDataAndWait(ctx, start)
		if !ready {
			return 0
		}
	default:
		snapshot, err = e.network.Inspect(ctx, previous)
	}
	if ctx.Err() != nil {
		return 0
	}
	if err != nil {
		e.updateAddress(netip.Addr{})
		e.transition(ctx, "error", "检查失败", err.Error())
		e.record("检查 IPv6", "失败", err.Error(), start)
		return 0
	}
	e.updateAddress(snapshot.Address)
	if !snapshot.Address.IsValid() {
		e.record("检查 IPv6", "地址缺失", snapshot.Reason, start)
		snapshot, err = e.recover(ctx, c, snapshot)
		if ctx.Err() != nil {
			return 0
		}
		if err != nil {
			e.transition(ctx, "waiting", "等待 IPv6", err.Error())
			return 0
		}
		e.updateAddress(snapshot.Address)
	} else {
		e.record("检查 IPv6", "成功", snapshot.Reason, start)
	}
	if !force && !c.Pending && c.RecordID != "" && c.Last.Address == snapshot.Address.String() && c.Last.TTL == c.TTL {
		reason := "本机 IPv6 和 TTL 与上次确认的云端记录一致，未发起更新"
		e.transition(ctx, "running", "运行中", reason)
		e.record("同步 AAAA", "跳过", reason, start)
		return 0
	}
	e.mu.Lock()
	retryAt, cloudError := e.retryAt, e.cloudError
	e.mu.Unlock()
	if !force && time.Until(retryAt) > 0 {
		if cloudError != "" {
			e.transition(ctx, "error", "同步失败", cloudError)
		} else {
			e.transition(ctx, "working", "同步中", "等待华为云确认记录集")
		}
		return time.Until(retryAt)
	}
	e.transition(ctx, "working", "同步中", c.Name())
	return e.sync(ctx, c, snapshot.Address)
}

func (e *Engine) enableDataAndWait(ctx context.Context, started time.Time) (android.Snapshot, bool) {
	e.transition(ctx, "working", "开启移动数据", "检测到移动数据已关闭，正在重新开启")
	err := e.network.SetData(ctx, true)
	if ctx.Err() != nil {
		return android.Snapshot{}, false
	}
	if err != nil {
		e.transition(ctx, "error", "开启移动数据失败", err.Error())
		e.record("开启移动数据", "失败", err.Error(), started)
		return android.Snapshot{}, false
	}
	reason := "已执行移动数据开启命令，等待蜂窝网络分配 IPv6"
	e.record("开启移动数据", "成功", reason, started)
	e.transition(ctx, "working", "等待 IPv6", reason)
	snapshot, err := e.waitAddress(ctx)
	if ctx.Err() != nil {
		return android.Snapshot{}, false
	}
	if err != nil {
		e.transition(ctx, "waiting", "等待 IPv6", err.Error())
		e.record("检查 IPv6", "等待", err.Error(), started)
		return android.Snapshot{}, false
	}
	return snapshot, true
}

func (e *Engine) sync(ctx context.Context, c Config, address netip.Addr) time.Duration {
	e.apiMu.Lock()
	defer e.apiMu.Unlock()
	if ctx.Err() != nil {
		return 0
	}
	start := time.Now()
	name := c.Name() + "."
	record, wrote, err := e.reconcile(ctx, c, name, address)
	if ctx.Err() != nil {
		return 0
	}
	if err != nil {
		e.mu.Lock()
		if ctx.Err() == nil {
			e.cloudFails = min(6, e.cloudFails+1)
			e.retryAt = time.Now().Add(min(5*time.Minute, 10*time.Second<<uint(e.cloudFails-1)))
			e.cloudError = err.Error()
		}
		e.mu.Unlock()
		e.transition(ctx, "error", "同步失败", err.Error())
		e.record("同步 AAAA", "失败", err.Error(), start)
		return 0
	}
	if record.ID == "" {
		e.transition(ctx, "error", "同步失败", "华为云未返回记录 ID")
		e.record("同步 AAAA", "失败", "华为云未返回记录 ID，将在下次检查时核对记录集", start)
		return 10 * time.Second
	}
	pending := strings.HasPrefix(record.Status, "PENDING_")
	e.mu.Lock()
	if ctx.Err() != nil {
		e.mu.Unlock()
		return 0
	}
	next := e.config
	next.RecordID = record.ID
	next.Pending = pending
	hadPendingWrite := c.PendingWrite && c.RecordID == record.ID
	applied := !pending && (wrote || hadPendingWrite)
	next.PendingWrite = pending && (wrote || hadPendingWrite)
	e.cloudFails, e.cloudError, e.retryAt = 0, "", time.Time{}
	if pending {
		e.retryAt = time.Now().Add(5 * time.Second)
	} else {
		next.Last.Address, next.Last.TTL = address.String(), c.TTL
		if applied {
			next.Last.At = time.Now().Unix()
		}
	}
	if next.RecordID != e.config.RecordID || next.Pending != e.config.Pending ||
		next.PendingWrite != e.config.PendingWrite || next.Last != e.config.Last {
		err = e.saveLocked(next)
	}
	e.mu.Unlock()
	if err != nil {
		e.transition(ctx, "error", "同步失败", err.Error())
		e.record("记录同步状态", "失败", err.Error(), start)
		return 0
	}
	if pending {
		outcome := "等待"
		if wrote {
			outcome = "已受理"
		}
		e.transition(ctx, "working", "同步中", "正在等待华为云确认记录集状态")
		e.record("同步 AAAA", outcome, c.Name()+" · 等待华为云生效", start)
		return 5 * time.Second
	}
	if !applied {
		reason := "云端 AAAA 与本机 IPv6 和配置 TTL 一致，未发起更新"
		e.transition(ctx, "running", "运行中", reason)
		e.record("同步 AAAA", "跳过", reason, start)
		return 0
	}
	e.transition(ctx, "running", "运行中", c.Name()+" → "+address.String())
	e.record("同步 AAAA", "成功", fmt.Sprintf("%s → %s · TTL %d 秒", c.Name(), address, c.TTL), start)
	return 0
}

func (e *Engine) reconcile(ctx context.Context, c Config, name string, address netip.Addr) (dns.Record, bool, error) {
	var record dns.Record
	if c.RecordID != "" {
		found, err := e.provider.Record(ctx, c.Key, c.Zone, c.RecordID)
		if err != nil && !errors.Is(err, dns.ErrNotFound) {
			return record, false, err
		}
		record = found
	}
	if record.ID == "" {
		records, err := e.provider.FindAAAA(ctx, c.Key, c.Zone, name)
		if err != nil {
			return record, false, err
		}
		switch {
		case len(records) == 0:
			created, err := e.provider.CreateAAAA(ctx, c.Key, c.Zone, name, address, c.TTL, c.Owner)
			return created, err == nil, err
		case len(records) == 1 && records[0].Description == c.Owner:
			record = records[0]
		default:
			e.mu.Lock()
			e.conflict = true
			e.mu.Unlock()
			return record, false, errors.New("检测到同名 AAAA 记录，请在配置页重新保存并确认选用")
		}
	}
	if record.Type != "AAAA" || !strings.EqualFold(strings.TrimSuffix(record.Name, "."), strings.TrimSuffix(name, ".")) ||
		record.Alias != nil || len(record.Values) > 1 {
		return record, false, errors.New("已绑定记录的名称、类型或内容已变化，请重新配置域名")
	}
	switch record.Status {
	case "PENDING_CREATE", "PENDING_UPDATE":
		return record, false, nil
	case "ACTIVE":
		if record.Matches(address, c.TTL) {
			return record, false, nil
		}
		updated, err := e.provider.UpdateAAAA(ctx, c.Key, c.Zone, record, address, c.TTL)
		return updated, err == nil, err
	default:
		return record, false, errors.New("AAAA 记录当前不可更新，请检查华为云中的记录状态")
	}
}

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (e *Engine) recover(ctx context.Context, c Config, snapshot android.Snapshot) (android.Snapshot, error) {
	start := time.Now()
	if time.Since(e.lastRecovery) < 5*time.Minute {
		reason := "地址仍缺失，移动数据重连处于 5 分钟冷却期"
		e.record("恢复 IPv6", "等待", reason, start)
		return snapshot, errors.New(reason)
	}
	radio := e.network.Radio(ctx)
	if !radio.DataKnown || !radio.AirplaneKnown || !radio.DataOn || radio.AirplaneOn {
		reason := "移动数据已关闭、飞行模式已开启或无法确认系统开关状态"
		e.record("恢复 IPv6", "暂停", reason, start)
		return snapshot, errors.New(reason)
	}
	if err := pause(ctx, 5*time.Second); err != nil {
		return snapshot, err
	}
	snapshot, err := e.network.Inspect(ctx, netip.Addr{})
	if err != nil || snapshot.Address.IsValid() {
		return snapshot, err
	}
	radio = e.network.Radio(ctx)
	if !radio.DataKnown || !radio.AirplaneKnown || !radio.DataOn || radio.AirplaneOn {
		return snapshot, errors.New("系统移动数据或飞行模式状态已变化")
	}
	e.lastRecovery = time.Now()
	e.transition(ctx, "working", "重连中", "连续两次缺少可用 IPv6，正在重连移动数据")
	e.record("恢复 IPv6", "开始", "连续两次缺少可用 IPv6，开始切换移动数据", start)
	if err := e.pulse(ctx, "data", 2*time.Second); err != nil {
		if ctx.Err() == nil {
			e.record("重连移动数据", "失败", err.Error(), start)
		}
		return snapshot, err
	}
	snapshot, err = e.waitAddress(ctx)
	if err == nil {
		e.record("重连移动数据", "成功", snapshot.Reason, start)
		return snapshot, nil
	}
	if ctx.Err() != nil {
		return snapshot, err
	}
	e.record("重连移动数据", "未恢复", err.Error(), start)
	if !c.Airplane {
		return snapshot, err
	}
	radio = e.network.Radio(ctx)
	if !radio.DataKnown || !radio.AirplaneKnown || !radio.DataOn || radio.AirplaneOn {
		return snapshot, errors.New("未执行飞行模式兜底，系统开关状态已变化")
	}
	start = time.Now()
	e.transition(ctx, "working", "重连中", "正在执行飞行模式兜底")
	if err := e.pulse(ctx, "airplane", 4*time.Second); err != nil {
		if ctx.Err() == nil {
			e.record("飞行模式兜底", "失败", err.Error(), start)
		}
		return snapshot, err
	}
	snapshot, err = e.waitAddress(ctx)
	if ctx.Err() != nil {
		return snapshot, ctx.Err()
	}
	if err != nil {
		e.record("飞行模式兜底", "未恢复", err.Error(), start)
	} else {
		e.record("飞行模式兜底", "成功", snapshot.Reason, start)
	}
	return snapshot, err
}

func (e *Engine) waitAddress(ctx context.Context) (android.Snapshot, error) {
	wait, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	for wait.Err() == nil {
		snapshot, err := e.network.Inspect(wait, netip.Addr{})
		if err == nil && snapshot.Address.IsValid() {
			return snapshot, nil
		}
		if err := pause(wait, 3*time.Second); err != nil {
			break
		}
	}
	if ctx.Err() != nil {
		return android.Snapshot{}, ctx.Err()
	}
	return android.Snapshot{}, errors.New("等待 50 秒后仍未取得蜂窝 INTERNET 全局 IPv6")
}

func (e *Engine) pulse(ctx context.Context, name string, duration time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	path := filepath.Join(e.dir, "radio-restore.json")
	if err := writeJSON(path, name); err != nil {
		return errors.New("无法记录网络开关恢复状态")
	}
	var err error
	if name == "data" {
		err = e.network.SetData(ctx, false)
	} else {
		err = e.network.SetAirplane(ctx, true)
	}
	if err == nil {
		err = pause(ctx, duration)
	}
	started := time.Now()
	restoreErr := e.restore(name, path)
	if restoreErr != nil && ctx.Err() != nil {
		e.record("恢复系统网络开关", "失败", restoreErr.Error(), started)
	}
	return errors.Join(err, restoreErr)
}

func (e *Engine) restore(name, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var err error
	for range 2 {
		if name == "data" {
			err = e.network.SetData(ctx, true)
		} else {
			err = e.network.SetAirplane(ctx, false)
		}
		if err == nil {
			return os.Remove(path)
		}
	}
	return errors.New("恢复系统网络开关失败，已保留恢复状态")
}

func (e *Engine) restorePending() {
	start := time.Now()
	path := filepath.Join(e.dir, "radio-restore.json")
	data, err := os.ReadFile(path)
	var kind string
	if err == nil && json.Unmarshal(data, &kind) == nil && (kind == "data" || kind == "airplane") {
		if err := e.restore(kind, path); err != nil {
			e.record("恢复遗留网络开关", "失败", err.Error(), start)
		} else {
			e.record("恢复遗留网络开关", "成功", "已恢复上次被中断的网络开关切换", start)
		}
	}
}
