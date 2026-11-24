package huawei

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"ipv6-ddns/internal/dns"
)

const Endpoint = "https://dns.cn-north-4.myhuaweicloud.com"

type Client struct {
	http     *http.Client
	endpoint string
}

func New(httpClient *http.Client) *Client {
	return &Client{http: httpClient, endpoint: Endpoint}
}

type apiError struct {
	Status int
	Code   string
}

func (e apiError) Error() string {
	reason := "华为云 DNS 请求失败"
	switch {
	case e.Status == 401:
		reason = "鉴权失败，请核对 AK、SK 和手机时间"
	case e.Status == 403:
		reason = "华为云拒绝请求，请核对密钥及 IAM 的 DNS 权限"
	case e.Status == 429:
		reason = "华为云 API 请求频率受限"
	case e.Status == 409:
		reason = "华为云报告记录冲突，请检查该子域名的现有记录"
	}
	if e.Code != "" {
		return fmt.Sprintf("%s（HTTP %d，%s）", reason, e.Status, e.Code)
	}
	return fmt.Sprintf("%s（HTTP %d）", reason, e.Status)
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
var cloudCode = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

func recordPath(version string, zone dns.Zone, recordID string) (string, error) {
	if !identifier.MatchString(zone.ID) || (recordID != "" && !identifier.MatchString(recordID)) {
		return "", errors.New("华为云返回了无法识别的资源 ID")
	}
	path := "/" + version + "/zones/" + zone.ID + "/recordsets"
	if recordID != "" {
		path += "/" + recordID
	}
	return path, nil
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func sign(req *http.Request, body []byte, credentials dns.Credentials, now time.Time) {
	const algorithm = "SDK-HMAC-SHA256"
	stamp := now.UTC().Format("20060102T150405Z")
	req.Header.Set("X-Sdk-Date", stamp)
	req.Header.Set("Content-Type", "application/json")
	path := req.URL.EscapedPath()
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	query := strings.ReplaceAll(req.URL.Query().Encode(), "+", "%20")
	headers := "content-type:application/json\nhost:" + req.URL.Host + "\nx-sdk-date:" + stamp + "\n"
	const signed = "content-type;host;x-sdk-date"
	canonical := strings.Join([]string{req.Method, path, query, headers, signed, digest(body)}, "\n")
	toSign := algorithm + "\n" + stamp + "\n" + digest([]byte(canonical))
	mac := hmac.New(sha256.New, []byte(credentials.SK))
	_, _ = mac.Write([]byte(toSign))
	req.Header.Set("Authorization", algorithm+" Access="+credentials.AK+", SignedHeaders="+signed+
		", Signature="+hex.EncodeToString(mac.Sum(nil)))
}

func (c *Client) request(ctx context.Context, key dns.Credentials, method, path string, query url.Values, input, output any) error {
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	u := c.endpoint + path
	if len(query) > 0 {
		u += "?" + strings.ReplaceAll(query.Encode(), "+", "%20")
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return errors.New("华为云请求构造失败")
	}
	sign(req, body, key, time.Now())
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Do not include signed URLs, response headers or credentials in errors.
		if strings.Contains(err.Error(), "解析") {
			return errors.New("无法解析华为云 DNS API 域名，请检查当前网络的 DNS")
		}
		return errors.New("无法通过 HTTPS 连接华为云 DNS API，请检查网络、系统证书和手机时间")
	}
	defer res.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode == http.StatusNotFound {
		return dns.ErrNotFound
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var payload struct {
			Code string `json:"error_code"`
		}
		_ = decoder.Decode(&payload)
		if !cloudCode.MatchString(payload.Code) {
			payload.Code = ""
		}
		return apiError{res.StatusCode, payload.Code}
	}
	if err := decoder.Decode(output); err != nil {
		return errors.New("华为云返回了无法读取的响应")
	}
	return nil
}

type links struct {
	Next string `json:"next"`
}

func advance(query url.Values, next string) (bool, error) {
	if next == "" {
		return false, nil
	}
	// Follow only the marker. Credentials are never sent to a host in a response link.
	u, err := url.Parse(next)
	if err != nil {
		return false, errors.New("华为云分页地址格式异常")
	}
	marker := u.Query().Get("marker")
	if !identifier.MatchString(marker) || marker == query.Get("marker") {
		return false, errors.New("华为云分页标记无效")
	}
	query.Set("marker", marker)
	return true, nil
}

func (c *Client) Zones(ctx context.Context, key dns.Credentials) ([]dns.Zone, error) {
	query := url.Values{"type": {"public"}, "limit": {"100"}}
	zones := []dns.Zone{}
	for range 100 {
		var result struct {
			Links links `json:"links"`
			Zones []struct {
				dns.Zone
				Status string `json:"status"`
			} `json:"zones"`
		}
		if err := c.request(ctx, key, http.MethodGet, "/v2/zones", query, nil, &result); err != nil {
			return nil, err
		}
		for _, row := range result.Zones {
			if row.Status == "ACTIVE" && identifier.MatchString(row.ID) && row.Name != "" {
				zones = append(zones, dns.Zone{ID: row.ID, Name: strings.TrimSuffix(strings.ToLower(row.Name), ".")})
			}
		}
		more, err := advance(query, result.Links.Next)
		if err != nil {
			return nil, err
		}
		if !more {
			sort.Slice(zones, func(i, j int) bool { return zones[i].Name < zones[j].Name })
			return zones, nil
		}
	}
	return nil, errors.New("公网域名分页超出查询上限")
}

func (c *Client) FindAAAA(ctx context.Context, key dns.Credentials, zone dns.Zone, name string) ([]dns.Record, error) {
	path, err := recordPath("v2.1", zone, "")
	if err != nil {
		return nil, err
	}
	query := url.Values{"name": {name}, "type": {"AAAA"}, "search_mode": {"equal"}, "limit": {"100"}}
	var list []dns.Record
	for range 100 {
		var result struct {
			Links   links        `json:"links"`
			Records []dns.Record `json:"recordsets"`
		}
		if err := c.request(ctx, key, http.MethodGet, path, query, nil, &result); err != nil {
			return nil, err
		}
		for _, rec := range result.Records {
			if rec.Type == "AAAA" && strings.EqualFold(strings.TrimSuffix(rec.Name, "."), strings.TrimSuffix(name, ".")) {
				list = append(list, rec)
			}
		}
		more, err := advance(query, result.Links.Next)
		if err != nil || !more {
			return list, err
		}
	}
	return nil, errors.New("记录集分页超出查询上限")
}

func (c *Client) Record(ctx context.Context, key dns.Credentials, zone dns.Zone, id string) (dns.Record, error) {
	path, err := recordPath("v2", zone, id)
	if err != nil {
		return dns.Record{}, err
	}
	var result dns.Record
	err = c.request(ctx, key, http.MethodGet, path, nil, nil, &result)
	return result, err
}

func (c *Client) CreateAAAA(ctx context.Context, key dns.Credentials, zone dns.Zone, name string, address netip.Addr, ttl int, description string) (dns.Record, error) {
	path, err := recordPath("v2", zone, "")
	if err != nil {
		return dns.Record{}, err
	}
	payload := map[string]any{"name": name, "description": description, "type": "AAAA", "ttl": ttl, "records": []string{address.String()}}
	var result dns.Record
	err = c.request(ctx, key, http.MethodPost, path, nil, payload, &result)
	return result, err
}

func (c *Client) UpdateAAAA(ctx context.Context, key dns.Credentials, zone dns.Zone, record dns.Record, address netip.Addr, ttl int) (dns.Record, error) {
	path, err := recordPath("v2", zone, record.ID)
	if err != nil {
		return dns.Record{}, err
	}
	payload := map[string]any{"ttl": ttl, "records": []string{address.String()}}
	var result dns.Record
	err = c.request(ctx, key, http.MethodPut, path, nil, payload, &result)
	return result, err
}
