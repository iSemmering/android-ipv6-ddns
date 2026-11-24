package android

import (
	"net/netip"
	"testing"
)

func TestSelectDefaultDataInternet(t *testing.T) {
	const dump = `Active default network: 104
  NetworkAgentInfo{network{104} ni{MOBILE CONNECTED} lp{{InterfaceName: rmnet_data2 }} nc{[ Transports: CELLULAR Capabilities: IMS&TRUSTED&VALIDATED Specifier: <TelephonyNetworkSpecifier [mSubId = 1]> SubscriptionIds: {1}]}}
  NetworkAgentInfo{network{107} ni{MOBILE CONNECTED} lp{{InterfaceName: rmnet_data1 }} nc{[ Transports: CELLULAR Capabilities: SUPL&INTERNET&VALIDATED Specifier: <TelephonyNetworkSpecifier [mSubId = 1]> SubscriptionIds: {1}]}}
  NetworkAgentInfo{network{110} ni{MOBILE CONNECTED} lp{{InterfaceName: rmnet_data3 }} nc{[ Transports: CELLULAR Capabilities: INTERNET&VALIDATED SubscriptionIds: {2}]}}
`
	if got := selectInterface(dump, "1"); got != "rmnet_data1" {
		t.Fatalf("got %q, want the INTERNET network for SIM 1", got)
	}
	if got := selectInterface(dump, "2"); got != "rmnet_data3" {
		t.Fatalf("got %q, want the INTERNET network for SIM 2", got)
	}
	if got := selectInterface(dump, "8"); got != "" {
		t.Fatalf("must not publish another subscription: %q", got)
	}
}

func TestAddressEligibility(t *testing.T) {
	const list = `    inet6 fd01::99/64 scope global
       valid_lft forever preferred_lft forever
    inet6 2409::1/64 scope global deprecated
       valid_lft 100sec preferred_lft 0sec
    inet6 2409::2/64 scope global tentative
       valid_lft forever preferred_lft forever
    inet6 2409::3/64 scope global temporary
       valid_lft 100sec preferred_lft 90sec
    inet6 2409::4/64 scope global dynamic mngtmpaddr
       valid_lft forever preferred_lft forever
    inet6 2409::5/64 scope global
       valid_lft 0sec preferred_lft 0sec`
	ip := selectAddress(list, netip.MustParseAddr("2409::3"))
	if ip.String() != "2409::4" {
		t.Fatalf("selected %v", ip)
	}
}
