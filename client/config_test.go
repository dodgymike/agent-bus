package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewRejectsMalformedBusURL exercises parseBusURL's fail-closed rules
// through the one public entry point that calls it eagerly, client.New, so a
// bad --bus is caught before any request is attempted.
func TestNewRejectsMalformedBusURL(t *testing.T) {
	tests := []struct {
		name          string
		url           string
		wantErrSubstr string
	}{
		{"userinfo", "http://user:pass@127.0.0.1:8080", "userinfo"},
		{"bad scheme", "ftp://127.0.0.1:8080", "scheme"},
		{"no scheme", "//127.0.0.1:8080", "scheme"},
		{"no host", "http://", "host"},
		{"query string", "http://127.0.0.1:8080?x=1", "query"},
		{"fragment", "http://127.0.0.1:8080#frag", "query"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(Config{BusURL: tt.url, IdentityDir: t.TempDir()})
			if err == nil {
				t.Fatalf("New(BusURL: %q) = nil error, want a usage error", tt.url)
			}
			if KindOf(err) != KindUsage {
				t.Fatalf("KindOf(err) = %q, want %q (err: %v)", KindOf(err), KindUsage, err)
			}
			if tt.wantErrSubstr != "" && !strings.Contains(err.Error(), tt.wantErrSubstr) {
				t.Fatalf("error %q does not mention %q", err.Error(), tt.wantErrSubstr)
			}
		})
	}
}

// TestParseBusURLTable exercises parseBusURL directly: non-loopback http is
// rejected (KindUsage), loopback http (IPv4, IPv6, "localhost") and https to
// any host are accepted, and the returned URL is CANONICALISED — host
// lower-cased, default port dropped. Canonicalisation matters because the
// returned string is the scope key idempotency records are compared against
// (see TestEnrolSameKeyCaseInsensitiveHostIsSameScope for an observable,
// end-to-end version of the same property).
func TestParseBusURLTable(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantErr       bool
		wantKind      Kind
		wantCanonical string // checked only when wantErr is false and non-empty
	}{
		{name: "http non-loopback rejected", raw: "http://example.com:8080", wantErr: true, wantKind: KindUsage},
		{name: "http non-loopback IP rejected", raw: "http://93.184.216.34:8080", wantErr: true, wantKind: KindUsage},
		{name: "http loopback IPv4 accepted", raw: "http://127.0.0.1:8080", wantErr: false, wantCanonical: "http://127.0.0.1:8080"},
		{name: "http loopback IPv6 accepted", raw: "http://[::1]:9090", wantErr: false, wantCanonical: "http://[::1]:9090"},
		{name: "http localhost accepted", raw: "http://localhost:9090", wantErr: false, wantCanonical: "http://localhost:9090"},
		{name: "https non-loopback accepted", raw: "https://example.com", wantErr: false, wantCanonical: "https://example.com"},
		{name: "https host case lowered", raw: "https://BUS:443/", wantErr: false, wantCanonical: "https://bus"},
		{name: "http default port dropped on loopback", raw: "http://127.0.0.1:80/", wantErr: false, wantCanonical: "http://127.0.0.1"},
		{name: "https non-default port kept", raw: "https://bus.example:8443/", wantErr: false, wantCanonical: "https://bus.example:8443"},
		{name: "trailing slash trimmed", raw: "https://bus.example/prefix/", wantErr: false, wantCanonical: "https://bus.example/prefix"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			u, err := parseBusURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseBusURL(%q) = nil error, want one", tt.raw)
				}
				if tt.wantKind != "" && KindOf(err) != tt.wantKind {
					t.Fatalf("KindOf(err) = %q, want %q (err: %v)", KindOf(err), tt.wantKind, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBusURL(%q): unexpected error: %v", tt.raw, err)
			}
			if tt.wantCanonical != "" && u.String() != tt.wantCanonical {
				t.Fatalf("parseBusURL(%q).String() = %q, want %q", tt.raw, u.String(), tt.wantCanonical)
			}
		})
	}
}

// TestCanonicalHostIPv6DefaultPort pins 2cf20abf: canonicalHost's default-port
// early return used to strip the brackets net.SplitHostPort removes and never
// put them back, turning "[::1]:443" into the bare "::1" — not a legal URL
// host, and this string scopes the idempotency store (the CANONICALISE
// comment on parseBusURL). The non-default-port path (net.JoinHostPort)
// already re-brackets correctly; this table exercises the branch that did
// not.
func TestCanonicalHostIPv6DefaultPort(t *testing.T) {
	tests := []struct {
		name   string
		scheme string
		host   string
		want   string
	}{
		{name: "https IPv6 default port loses its brackets", scheme: "https", host: "[::1]:443", want: "[::1]"},
		{name: "http IPv6 default port loses its brackets", scheme: "http", host: "[::1]:80", want: "[::1]"},
		{name: "https IPv6 global literal default port loses its brackets", scheme: "https", host: "[2001:db8::1]:443", want: "[2001:db8::1]"},
		// Explicit (non-default) port: net.JoinHostPort already re-brackets
		// correctly. Included so a future edit that "simplifies" both branches
		// into one has a red flag on this side too if it regresses.
		{name: "https IPv6 explicit port stays bracketed", scheme: "https", host: "[::1]:9090", want: "[::1]:9090"},
		{name: "http IPv6 explicit port stays bracketed", scheme: "http", host: "[2001:db8::1]:8080", want: "[2001:db8::1]:8080"},
		// No port at all: SplitHostPort's error path returns the input
		// lower-cased and unchanged — url.URL.Host already carries the
		// brackets for a bare IPv6 host, so this must stay bracketed too.
		{name: "https IPv6 no port stays bracketed", scheme: "https", host: "[::1]", want: "[::1]"},
		// IPv4 and named hosts are unaffected by the bracket fix.
		{name: "http IPv4 default port drops cleanly", scheme: "http", host: "127.0.0.1:80", want: "127.0.0.1"},
		{name: "https named host default port drops cleanly", scheme: "https", host: "BUS:443", want: "bus"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalHost(tt.scheme, tt.host); got != tt.want {
				t.Fatalf("canonicalHost(%q, %q) = %q, want %q", tt.scheme, tt.host, got, tt.want)
			}
		})
	}
}

// TestBusURLPathPrefixIsKeptAndTrailingSlashTrimmed checks a base URL that
// carries a path prefix is honoured on every request, and a trailing slash
// does not turn into a doubled slash when a route path is appended.
func TestBusURLPathPrefixIsKeptAndTrailingSlashTrimmed(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(enrolResponseBody{
			AgentID: "bus-x.a-1", BusID: "bus-x", Name: "a",
			EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}))
	defer srv.Close()

	c, err := New(Config{BusURL: srv.URL + "/prefix/", IdentityDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Enrol(context.Background(), EnrolOptions{Name: "a", Save: true}); err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	want := "/prefix" + routeEnroll
	if gotPath != want {
		t.Fatalf("request path = %q, want %q", gotPath, want)
	}
}
