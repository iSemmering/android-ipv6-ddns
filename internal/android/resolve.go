package android

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var dnsServers = regexp.MustCompile(`DnsAddresses:\s*\[([^\]]*)\]`)

// Use Android's default resolver so Private DNS and per-network DNS work
// even on devices where there is no /etc/resolv.conf.
func netdLookup(ctx context.Context, host, port string) ([]netip.Addr, error) {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", "/dev/socket/dnsproxyd")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}
	cancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer cancel()
	if _, err = fmt.Fprintf(conn, "getaddrinfo %s %s 0 0 1 6 0%c", host, port, 0); err != nil {
		return nil, err
	}
	var code [4]byte
	if _, err = io.ReadFull(conn, code[:]); err != nil || string(code[:3]) != "222" {
		return nil, errors.New("系统 DNS 解析未返回地址")
	}
	return readNetdResults(conn)
}

func readNetdResults(reader io.Reader) ([]netip.Addr, error) {
	var addresses []netip.Addr
	for range 64 {
		var more uint32
		if err := binary.Read(reader, binary.BigEndian, &more); err != nil {
			return nil, err
		}
		if more == 0 {
			return addresses, nil
		}
		var fields [5]uint32
		if err := binary.Read(reader, binary.BigEndian, &fields); err != nil {
			return nil, err
		}
		if fields[4] > 128 {
			return nil, errors.New("系统 DNS 地址响应格式异常")
		}
		data := make([]byte, fields[4])
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, err
		}
		switch {
		case fields[1] == 2 && len(data) >= 8:
			addresses = append(addresses, netip.AddrFrom4([4]byte(data[4:8])))
		case fields[1] == 10 && len(data) >= 24:
			addresses = append(addresses, netip.AddrFrom16([16]byte(data[8:24])).Unmap())
		}
		var nameLength uint32
		if err := binary.Read(reader, binary.BigEndian, &nameLength); err != nil {
			return nil, err
		}
		if nameLength > 1024 {
			return nil, errors.New("系统 DNS 名称响应格式异常")
		}
		if _, err := io.CopyN(io.Discard, reader, int64(nameLength)); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("系统 DNS 返回地址过多")
}

func linkDNSLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	text, err := run(ctx, "dumpsys", "connectivity")
	if err != nil {
		return nil, err
	}
	active := first(defaultNet, text)
	for _, block := range agents(text) {
		if active == "" || first(networkID, block) != active {
			continue
		}
		for _, item := range strings.Split(first(dnsServers, block), ",") {
			server, err := netip.ParseAddr(strings.Trim(strings.TrimSpace(item), "/"))
			if err != nil {
				continue
			}
			dialer := net.Dialer{Timeout: 2 * time.Second}
			resolver := &net.Resolver{PreferGo: true, Dial: func(c context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(c, network, net.JoinHostPort(server.String(), "53"))
			}}
			lookup, cancel := context.WithTimeout(ctx, 3*time.Second)
			addrs, err := resolver.LookupNetIP(lookup, "ip", host+".")
			cancel()
			if err == nil && len(addrs) > 0 {
				return addrs, nil
			}
		}
		break
	}
	return nil, errors.New("当前网络无法解析华为云 DNS API 域名")
}

func dialAndroid(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, _ := netdLookup(ctx, host, port)
	if len(ips) == 0 {
		ips, err = linkDNSLookup(ctx, host)
		if err != nil {
			return nil, err
		}
	}
	dialer := net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("无法连接华为云 DNS API")
}

func HTTPClient() *http.Client {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	for _, dir := range []string{"/apex/com.android.conscrypt/cacerts", "/system/etc/security/cacerts"} {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if certificate, err := os.ReadFile(filepath.Join(dir, entry.Name())); err == nil {
				roots.AppendCertsFromPEM(certificate)
			}
		}
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: dialAndroid, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 7 * time.Second, IdleConnTimeout: 60 * time.Second,
		},
		Timeout:       18 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
