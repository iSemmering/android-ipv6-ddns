package huawei

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"ipv6-ddns/internal/dns"
)

func TestSignedPaginatedAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 Access=testAK, SignedHeaders=content-type;host;x-sdk-date, Signature=") ||
			r.Header.Get("X-Sdk-Date") == "" {
			t.Error("missing SDK HMAC signature")
		}
		switch {
		case r.URL.Path == "/v2/zones" && r.URL.Query().Get("marker") == "":
			fmt.Fprint(w, `{"zones":[{"id":"z1","name":"example.com.","status":"ACTIVE"}],"links":{"next":"https://untrusted.invalid/?marker=z1"}}`)
		case r.URL.Path == "/v2/zones":
			fmt.Fprint(w, `{"zones":[{"id":"z2","name":"example.net.","status":"ACTIVE"},{"id":"z3","name":"inactive.test.","status":"FREEZE"}]}`)
		case r.URL.Path == "/v2.1/zones/z1/recordsets":
			if r.URL.Query().Get("name") != "phone.example.com." || r.URL.Query().Get("type") != "AAAA" {
				t.Error("missing record lookup filters")
			}
			fmt.Fprint(w, `{"recordsets":[{"id":"r1","name":"phone.example.com.","type":"AAAA","records":["2409::1"],"status":"ACTIVE","ttl":1},{"id":"r2","name":"else.example.com.","type":"AAAA"}]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/zones/z1/recordsets/r1":
			var body struct {
				TTL     int      `json:"ttl"`
				Records []string `json:"records"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.TTL != 33 || len(body.Records) != 1 || body.Records[0] != "2409::2" {
				t.Error("update should contain one IPv6 and the requested TTL")
			}
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id":"r1","status":"PENDING_UPDATE"}`)
		default:
			t.Errorf("unexpected API call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := &Client{http: server.Client(), endpoint: server.URL}
	key := dns.Credentials{AK: "testAK", SK: "testSK"}
	zones, err := client.Zones(context.Background(), key)
	if err != nil || len(zones) != 2 || zones[0].Name != "example.com" {
		t.Fatalf("zones %+v, %v", zones, err)
	}
	records, err := client.FindAAAA(context.Background(), key, zones[0], "phone.example.com.")
	if err != nil || len(records) != 1 {
		t.Fatalf("records %+v, %v", records, err)
	}
	updated, err := client.UpdateAAAA(context.Background(), key, zones[0], records[0], netip.MustParseAddr("2409::2"), 33)
	if err != nil || updated.Status != "PENDING_UPDATE" {
		t.Fatalf("update %+v, %v", updated, err)
	}
}
