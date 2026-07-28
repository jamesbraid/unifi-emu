package herder

import "testing"

func TestInformURLAcceptsACanonicalIPv4Endpoint(t *testing.T) {
	for _, raw := range []string{
		"http://172.28.0.2:8080/inform",
		"http://10.0.0.1:1/inform",
		"http://192.168.1.250:65535/inform",
	} {
		if err := CheckInformURL(raw); err != nil {
			t.Errorf("CheckInformURL(%q) = %v, want nil", raw, err)
		}
	}
}

func TestInformURLRejectsEverythingOutsideTheContract(t *testing.T) {
	bad := map[string]string{
		"a hostname":              "http://controller:8080/inform",
		"host.docker.internal":    "http://host.docker.internal:8080/inform",
		"an IPv6 literal":         "http://[fd00::2]:8080/inform",
		"loopback":                "http://127.0.0.1:8080/inform",
		"the unspecified address": "http://0.0.0.0:8080/inform",
		"a leading-zero octet":    "http://172.028.0.2:8080/inform",
		"a missing port":          "http://172.28.0.2/inform",
		"port zero":               "http://172.28.0.2:0/inform",
		"a port above 65535":      "http://172.28.0.2:65536/inform",
		"a non-numeric port":      "http://172.28.0.2:http/inform",
		"https":                   "https://172.28.0.2:8080/inform",
		"another path":            "http://172.28.0.2:8080/inform/v2",
		"no path":                 "http://172.28.0.2:8080",
		"user information":        "http://user:pass@172.28.0.2:8080/inform",
		"a query":                 "http://172.28.0.2:8080/inform?site=default",
		"a fragment":              "http://172.28.0.2:8080/inform#frag",
		"empty":                   "",
		"garbage":                 "://",
	}
	for what, raw := range bad {
		err := CheckInformURL(raw)
		if err == nil {
			t.Errorf("CheckInformURL accepted %s (%q)", what, raw)
			continue
		}
		assertFailure(t, err, CodeInformURLInvalid, PhaseValidate)
	}
}
