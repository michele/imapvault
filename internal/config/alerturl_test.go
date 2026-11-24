package config

import (
	"strings"
	"testing"
)

const secretTopic = "SECRETTOPIC123"

func withGuard(extra string) string { return validBase + "\n[guard]\n" + extra + "\n" }

func TestAlertURLPolicyInConfig(t *testing.T) {
	cases := []struct {
		url string
		ok  bool
	}{
		{"https://ntfy.sh/" + secretTopic, true},
		{"https://user:pw@ntfy.example.org/" + secretTopic, true},
		{"http://127.0.0.1:8080/" + secretTopic, true},
		{"http://[::1]:8080/" + secretTopic, true},
		{"http://localhost/" + secretTopic, true},
		{"http://192.168.1.5/" + secretTopic, true},
		{"http://10.0.0.7:2586/" + secretTopic, true},
		{"http://169.254.1.1/" + secretTopic, true},
		{"http://ntfy/" + secretTopic, true},           // a compose service name
		{"http://ntfy.sh/" + secretTopic, false},       // public host in clear text
		{"http://8.8.8.8/" + secretTopic, false},       // public IP in clear text
		{"ftp://ntfy.sh/" + secretTopic, false},        // not http(s)
		{"ntfy.sh/" + secretTopic, false},              // no scheme
		{"https:///" + secretTopic, false},             // no host
		{"https://ntfy.sh/ bad " + secretTopic, false}, // not a URL
	}
	for _, c := range cases {
		t.Run(c.url, func(t *testing.T) {
			_, err := Parse([]byte(withGuard(`on_alert_url = "`+c.url+`"`)), pw)
			if (err == nil) != c.ok {
				t.Fatalf("ok=%v, got err %v", c.ok, err)
			}
			if err != nil && strings.Contains(err.Error(), secretTopic) {
				t.Errorf("the error must not echo the URL: %v", err)
			}
		})
	}
}

func TestAlertURLAndFileAreExclusive(t *testing.T) {
	_, err := Parse([]byte(withGuard("on_alert_url = \"https://ntfy.sh/x\"\non_alert_url_file = \"/run/secrets/alert_url\"")), pw)
	if err == nil || !strings.Contains(err.Error(), "on_alert_url") {
		t.Fatalf("got %v", err)
	}
}

func TestAlertURLFromFile(t *testing.T) {
	cfg, err := Parse([]byte(withGuard(`on_alert_url_file = "/run/secrets/alert_url"`)), pw)
	if err != nil {
		t.Fatal(err)
	}
	const path = "/run/secrets/alert_url"
	cases := []struct {
		name    string
		fs      fakeFS
		env     map[string]string
		want    string
		warn    bool
		wantErr string
	}{
		{"read and trimmed", fakeFS{path: {data: "https://ntfy.sh/" + secretTopic + "\n", mode: 0o600}}, nil, "https://ntfy.sh/" + secretTopic, false, ""},
		{"missing", fakeFS{}, nil, "", false, "no such file"},
		{"empty", fakeFS{path: {data: "\n", mode: 0o600}}, nil, "", false, "empty"},
		{"two lines", fakeFS{path: {data: "https://a.example/x\nhttps://b.example/y\n", mode: 0o600}}, nil, "", false, "one line"},
		{"readable by others outside a container", fakeFS{path: {data: "https://ntfy.sh/x\n", mode: 0o644}}, nil, "", false, "readable by other users"},
		{"readable by others inside a container only warns", fakeFS{path: {data: "https://ntfy.sh/x\n", mode: 0o644}, "/.dockerenv": {}}, nil, "https://ntfy.sh/x", true, ""},
		{"the policy applies to the file's content", fakeFS{path: {data: "http://ntfy.sh/" + secretTopic + "\n", mode: 0o600}}, nil, "", false, "https"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, warns, err := cfg.alertURL(c.fs.deps(c.env))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("want error containing %q, got %v", c.wantErr, err)
				}
				if strings.Contains(err.Error(), secretTopic) {
					t.Errorf("the error echoes the URL: %v", err)
				}
				return
			}
			if err != nil || got != c.want || (len(warns) > 0) != c.warn {
				t.Fatalf("got %q, %v, %v; want %q (warn %v)", got, warns, err, c.want, c.warn)
			}
		})
	}
}

func TestAlertURLNoneConfigured(t *testing.T) {
	cfg, err := Parse([]byte(validBase), pw)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := cfg.AlertURL()
	if err != nil || got != "" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestAlertURLDirectValue(t *testing.T) {
	cfg, err := Parse([]byte(withGuard(`on_alert_url = "https://ntfy.sh/`+secretTopic+`"`)), pw)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := cfg.AlertURL()
	if err != nil || got != "https://ntfy.sh/"+secretTopic {
		t.Fatalf("got %q, %v", got, err)
	}
}
