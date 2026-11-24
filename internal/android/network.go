package android

import (
	"context"
	"errors"
	"net/netip"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Network struct{}

type Snapshot struct {
	Interface string
	Address   netip.Addr
	Reason    string
}

type Radio struct {
	DataOn, AirplaneOn       bool
	DataKnown, AirplaneKnown bool
}

var (
	agent         = regexp.MustCompile(`(?m)^[ \t]*NetworkAgentInfo(?:\{|\s*\[)`)
	iface         = regexp.MustCompile(`InterfaceName\s*[:=]\s*([\w.-]+)`)
	transports    = regexp.MustCompile(`Transports:\s*(.*?)\s+Capabilities:`)
	capabilities  = regexp.MustCompile(`Capabilities:\s*([A-Z0-9_& |]+)`)
	networkID     = regexp.MustCompile(`(?:network\{|NetId[=: ]+| - )(\d+)\b`)
	defaultNet    = regexp.MustCompile(`(?m)^\s*Active default network:\s*(\d+)\b`)
	subscriptions = regexp.MustCompile(`SubscriptionIds\s*[:=]?\s*\{([^}]*)\}`)
	subSpecifier  = regexp.MustCompile(`(?:mSubId|SubscriptionId)\s*=\s*(\d+)`)
	activeSub     = regexp.MustCompile(`(?m)^\s*mActiveDataSubId\s*=\s*(\d+)`)
	ipLine        = regexp.MustCompile(`^\s*inet6\s+([^\s]+)\s+(.*)`)
	docIPv6       = netip.MustParsePrefix("2001:db8::/32")
	globalIPv6    = netip.MustParsePrefix("2000::/3")
)

func run(ctx context.Context, name string, args ...string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	out, err := exec.CommandContext(bounded, "/system/bin/"+name, args...).CombinedOutput()
	if bounded.Err() != nil {
		return "", bounded.Err()
	}
	return strings.TrimSpace(string(out)), err
}

func setting(ctx context.Context, key string) string {
	value, _ := run(ctx, "settings", "get", "global", key)
	return value
}

func defaultSubscription(ctx context.Context) string {
	value := setting(ctx, "multi_sim_data_call")
	if id, err := strconv.Atoi(value); err == nil && id >= 0 {
		return value
	}
	text, _ := run(ctx, "dumpsys", "telephony.registry")
	return first(activeSub, text)
}

func first(re *regexp.Regexp, value string) string {
	match := re.FindStringSubmatch(value)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

func hasToken(list, token string) bool {
	return slices.Contains(strings.FieldsFunc(list, func(r rune) bool {
		return r == '&' || r == '|' || r == ' ' || r == ',' || r == '\t'
	}), token)
}

func agents(text string) []string {
	starts := agent.FindAllStringIndex(text, -1)
	out := make([]string, 0, len(starts))
	for i, pos := range starts {
		end := len(text)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		// Each agent is normally printed on one line. Limit wrapped entries to their section.
		block := strings.SplitN(text[pos[0]:end], "\n\n", 2)[0]
		out = append(out, block)
	}
	return out
}

func selectInterface(text, sub string) string {
	active := first(defaultNet, text)
	bestScore, best := -1, ""
	for _, block := range agents(text) {
		tr := first(transports, block)
		caps := first(capabilities, block)
		if !hasToken(tr, "CELLULAR") || !hasToken(caps, "INTERNET") {
			continue
		}
		name := first(iface, block)
		if name == "" || strings.Contains(block, "DISCONNECTED") {
			continue
		}
		ids := first(subscriptions, block)
		if ids == "" {
			ids = first(subSpecifier, block)
		}
		if sub != "" && ids != "" && !hasToken(ids, sub) {
			continue
		}
		score := 0
		if active != "" && active == first(networkID, block) {
			score += 20
		}
		if sub != "" && hasToken(ids, sub) {
			score += 40
		}
		if hasToken(caps, "VALIDATED") {
			score += 8
		}
		if score > bestScore {
			bestScore, best = score, name
		}
	}
	return best
}

func selectAddress(text string, previous netip.Addr) netip.Addr {
	bestScore, best := -1, netip.Addr{}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		match := ipLine.FindStringSubmatch(line)
		if len(match) == 0 || !strings.Contains(match[2], "scope global") {
			continue
		}
		prefix, err := netip.ParsePrefix(match[1])
		if err != nil || !globalIPv6.Contains(prefix.Addr()) || docIPv6.Contains(prefix.Addr()) {
			continue
		}
		flags := strings.Fields(match[2])
		if slices.Contains(flags, "deprecated") || slices.Contains(flags, "tentative") ||
			slices.Contains(flags, "dadfailed") || slices.Contains(flags, "optimistic") {
			continue
		}
		if i+1 < len(lines) && (strings.Contains(lines[i+1], "valid_lft 0sec") ||
			strings.Contains(lines[i+1], "preferred_lft 0sec")) {
			continue
		}
		score := 1
		if !slices.Contains(flags, "temporary") {
			score += 10
		}
		if prefix.Addr() == previous {
			score += 2
		}
		if score > bestScore || (score == bestScore && prefix.Addr().Less(best)) {
			bestScore, best = score, prefix.Addr()
		}
	}
	return best
}

func (Network) Inspect(ctx context.Context, previous netip.Addr) (Snapshot, error) {
	sub := defaultSubscription(ctx)
	text, err := run(ctx, "dumpsys", "connectivity")
	if err != nil || (!strings.Contains(text, "NetworkAgentInfo") && !strings.Contains(text, "Active default network:")) {
		return Snapshot{}, errors.New("读取 Android 网络能力失败")
	}
	name := selectInterface(text, sub)
	if name == "" {
		return Snapshot{Reason: "默认数据卡未建立蜂窝 INTERNET 网络"}, nil
	}
	lines, err := run(ctx, "ip", "-6", "addr", "show", "dev", name, "scope", "global")
	if err != nil {
		return Snapshot{}, errors.New("读取蜂窝接口 IPv6 失败")
	}
	addr := selectAddress(lines, previous)
	reason := name + " 上暂无可用全局 IPv6"
	if addr.IsValid() {
		reason = name + " · CELLULAR + INTERNET · " + addr.String()
	}
	return Snapshot{Interface: name, Address: addr, Reason: reason}, nil
}

func (Network) Radio(ctx context.Context) Radio {
	sub := defaultSubscription(ctx)
	mobile := ""
	if sub != "" {
		mobile = setting(ctx, "mobile_data"+sub)
	}
	if mobile != "0" && mobile != "1" {
		mobile = setting(ctx, "mobile_data")
	}
	airplane := setting(ctx, "airplane_mode_on")
	return Radio{
		DataOn: mobile == "1", DataKnown: mobile == "0" || mobile == "1",
		AirplaneOn: airplane == "1", AirplaneKnown: airplane == "0" || airplane == "1",
	}
}

func action(enable bool) string {
	if enable {
		return "enable"
	}
	return "disable"
}

func succeeded(out string, err error) bool {
	lower := strings.ToLower(out)
	return err == nil && !strings.Contains(lower, "unknown command") &&
		!strings.Contains(lower, "securityexception") && !strings.Contains(lower, "not found")
}

func (Network) SetData(ctx context.Context, enable bool) error {
	out, err := run(ctx, "svc", "data", action(enable))
	if !succeeded(out, err) {
		return errors.New("Android 移动数据切换命令失败")
	}
	return nil
}

func (Network) SetAirplane(ctx context.Context, enable bool) error {
	out, err := run(ctx, "cmd", "connectivity", "airplane-mode", action(enable))
	if succeeded(out, err) {
		return nil
	}
	text := strings.ToLower(out)
	if !strings.Contains(text, "unknown command") && !strings.Contains(text, "no shell command") {
		return errors.New("执行系统飞行模式切换命令失败")
	}
	value := "0"
	if enable {
		value = "1"
	}
	if _, err := run(ctx, "settings", "put", "global", "airplane_mode_on", value); err != nil {
		return errors.New("写入系统飞行模式状态失败")
	}
	out, err = run(ctx, "am", "broadcast", "-a", "android.intent.action.AIRPLANE_MODE", "--ez", "state", strconv.FormatBool(enable))
	if !succeeded(out, err) || strings.Contains(out, "Exception") {
		return errors.New("系统拒绝了飞行模式状态广播")
	}
	return nil
}

func Timezone(ctx context.Context) *time.Location {
	name, _ := run(ctx, "getprop", "persist.sys.timezone")
	if loc, err := time.LoadLocation(name); name != "" && err == nil {
		return loc
	}
	return time.Local
}
