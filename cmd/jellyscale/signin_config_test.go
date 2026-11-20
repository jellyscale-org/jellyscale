package main

import (
	"strings"
	"testing"
)

// JS-145 (D-020): `trustedProxies` is a list of IP addresses or CIDR prefixes. A bad entry stops start-up with an error that
// names the key (every role parses the one config, so none starts with it). Absent and [] are different states; that is
// held by the process checks of signin_more_integration_test.go (the start-up warning and the metric).

func TestTrustedProxiesTakesAddressesAndPrefixes(t *testing.T) {
	for _, good := range []string{`[]`, `["127.0.0.1"]`, `["10.0.0.0/8", "172.16.0.0/12", "::1", "2001:db8::/32", "fd00::1"]`} {
		if _, err := parseConfig([]byte(`{"trustedProxies": ` + good + `}`)); err != nil {
			t.Errorf("trustedProxies %s: %v, want it accepted", good, err)
		}
	}
}

func TestABadTrustedProxiesEntryStopsStartUpNamingTheKey(t *testing.T) {
	for _, bad := range []string{`["proxy.example"]`, `["10.0.0.0/33"]`, `["999.1.1.1"]`, `[""]`, `["10.0.0.1", "nope"]`, `[5]`, `"10.0.0.1"`, `["::1/129"]`} {
		_, err := parseConfig([]byte(`{"trustedProxies": ` + bad + `}`))
		switch {
		case err == nil:
			t.Errorf("trustedProxies %s was accepted", bad)
		case !strings.Contains(err.Error(), "trustedProxies") || strings.Contains(err.Error(), "unknown field"):
			t.Errorf("trustedProxies %s: %q, want an error that names the key as one this build knows", bad, err)
		}
	}
}
