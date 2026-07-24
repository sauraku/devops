package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTrustedProxyCIDRs(t *testing.T) {
	prefixes, err := parseTrustedProxyCIDRs("127.0.0.1, 10.0.0.0/8, ::ffff:192.0.2.0/120, ::1")
	if err != nil {
		t.Fatalf("parse valid trusted proxies: %v", err)
	}
	want := []string{"127.0.0.1/32", "10.0.0.0/8", "192.0.2.0/24", "::1/128"}
	if len(prefixes) != len(want) {
		t.Fatalf("got %d prefixes, want %d", len(prefixes), len(want))
	}
	for i := range want {
		if got := prefixes[i].String(); got != want[i] {
			t.Errorf("prefix[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestParseTrustedProxyCIDRsRejectsMalformedEntry(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1,not-a-network",
		"192.0.2.1/255.255.255.0",
		"::ffff:192.0.2.0/95",
	} {
		if _, err := parseTrustedProxyCIDRs(raw); err == nil {
			t.Errorf("malformed trusted proxy entry %q was accepted", raw)
		}
	}
}

func TestValidateTrustedProxyCommand(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		raw     string
		handled bool
		wantErr bool
	}{
		{name: "normal startup", handled: false},
		{name: "valid", args: []string{"validate-trusted-proxy-cidrs"}, raw: "127.0.0.1,10.0.0.0/8", handled: true},
		{name: "empty is valid", args: []string{"validate-trusted-proxy-cidrs"}, handled: true},
		{name: "invalid", args: []string{"validate-trusted-proxy-cidrs"}, raw: "192.0.2.1/255.255.255.0", handled: true, wantErr: true},
		{name: "extra argument", args: []string{"validate-trusted-proxy-cidrs", "unexpected"}, raw: "127.0.0.1", handled: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handled, err := validateTrustedProxyCommand(test.args, test.raw)
			if handled != test.handled {
				t.Fatalf("handled = %v, want %v", handled, test.handled)
			}
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestValidateComposeSourceCommand(t *testing.T) {
	root := t.TempDir()
	composePath := filepath.Join(root, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  app:\n    image: example/app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if handled, err := validateComposeSourceCommand(nil); handled || err != nil {
		t.Fatalf("normal startup handled=%v err=%v", handled, err)
	}
	if handled, err := validateComposeSourceCommand([]string{"validate-compose-source", composePath, root}); !handled || err != nil {
		t.Fatalf("valid Compose command handled=%v err=%v", handled, err)
	}
	if err := os.WriteFile(composePath, []byte("services:\n  app:\n    env_file: /opt/devops-control/.env.prod\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if handled, err := validateComposeSourceCommand([]string{"validate-compose-source", composePath, root}); !handled || err == nil {
		t.Fatalf("unsafe Compose command handled=%v err=%v", handled, err)
	}
}
