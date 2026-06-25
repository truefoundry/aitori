//go:build darwin

package adapter

import (
	"reflect"
	"testing"
)

func TestParseNetworkServices(t *testing.T) {
	out := `An asterisk (*) denotes that a network service is disabled.
Wi-Fi
Thunderbolt Bridge
*Disabled Service
USB 10/100/1000 LAN
`
	got := parseNetworkServices(out)
	want := []string{"Wi-Fi", "Thunderbolt Bridge", "USB 10/100/1000 LAN"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseNetworkServices = %v, want %v", got, want)
	}
}

func TestParseNetworkServicesEmpty(t *testing.T) {
	if got := parseNetworkServices("An asterisk (*) denotes that a network service is disabled.\n"); len(got) != 0 {
		t.Errorf("expected no services, got %v", got)
	}
}
