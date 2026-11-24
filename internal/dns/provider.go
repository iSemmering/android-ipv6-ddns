package dns

import (
	"context"
	"errors"
	"net/netip"
)

type Credentials struct {
	AK string `json:"ak"`
	SK string `json:"sk"`
}

type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Record struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Status      string   `json:"status"`
	TTL         int      `json:"ttl"`
	Values      []string `json:"records"`
	Description string   `json:"description"`
	Alias       any      `json:"alias_target"`
}

func (r Record) Matches(ip netip.Addr, ttl int) bool {
	if len(r.Values) != 1 || r.TTL != ttl {
		return false
	}
	current, err := netip.ParseAddr(r.Values[0])
	return err == nil && current == ip
}

var ErrNotFound = errors.New("云端记录集不存在")

type Provider interface {
	Zones(context.Context, Credentials) ([]Zone, error)
	FindAAAA(context.Context, Credentials, Zone, string) ([]Record, error)
	Record(context.Context, Credentials, Zone, string) (Record, error)
	CreateAAAA(context.Context, Credentials, Zone, string, netip.Addr, int, string) (Record, error)
	UpdateAAAA(context.Context, Credentials, Zone, Record, netip.Addr, int) (Record, error)
}
