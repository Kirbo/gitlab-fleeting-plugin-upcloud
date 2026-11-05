package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"testing"

	"github.com/hashicorp/go-hclog"
)

// setVersion overrides the build version for the duration of a test.
func setVersion(t *testing.T, v string) {
	t.Helper()
	orig := Version.Version
	Version.Version = v
	t.Cleanup(func() { Version.Version = orig })
}

var guidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestTelemetrySessionID(t *testing.T) {
	id := telemetrySessionID("alice", "ucat_secret")
	if !guidRe.MatchString(id) {
		t.Errorf("session id %q is not GUID-shaped", id)
	}
	if again := telemetrySessionID("alice", "ucat_secret"); again != id {
		t.Errorf("session id not deterministic: %q != %q", again, id)
	}
	if other := telemetrySessionID("alice", "ucat_other"); other == id {
		t.Error("different keys produced the same session id")
	}
	if other := telemetrySessionID("bob", "ucat_secret"); other == id {
		t.Error("different usernames produced the same session id")
	}
}

func TestReleaseChannel(t *testing.T) {
	for v, want := range map[string]string{
		"1.4.0":        "stable",
		"1.5.0-rc.1":   "beta",
		"2.0.0-beta.2": "beta",
	} {
		if got := releaseChannel(v); got != want {
			t.Errorf("releaseChannel(%q) = %q, want %q", v, got, want)
		}
	}
}

func TestTelemetryEnabled(t *testing.T) {
	off, on := false, true
	tests := []struct {
		name      string
		version   string
		telemetry *bool
		want      bool
	}{
		{"default on", "v1.4.0", nil, true},
		{"explicitly on", "v1.4.0", &on, true},
		{"opted out", "v1.4.0", &off, false},
		{"dev build", "dev", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setVersion(t, tt.version)
			g := &InstanceGroup{Telemetry: tt.telemetry}
			if got := g.telemetryEnabled(); got != tt.want {
				t.Errorf("telemetryEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTelemetryEnabled_NoAppKey(t *testing.T) {
	setVersion(t, "v1.4.0")
	orig := aptabaseAppKey
	aptabaseAppKey = ""
	t.Cleanup(func() { aptabaseAppKey = orig })

	if (&InstanceGroup{}).telemetryEnabled() {
		t.Error("telemetryEnabled() = true without an App-Key")
	}
}

func TestTelemetrySend(t *testing.T) {
	setVersion(t, "v1.5.0-rc.1")

	var (
		gotHeader http.Header
		gotEvents []aptabaseEvent
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotEvents); err != nil {
			t.Errorf("decoding body %s: %v", body, err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	orig := telemetryURL
	telemetryURL = srv.URL
	t.Cleanup(func() { telemetryURL = orig })

	g := &InstanceGroup{Token: "ucat_secret", Zone: "fi-hel1", Plan: "4xCPU-8GB", log: hclog.NewNullLogger()}
	if err := g.newTelemetry("alice").send(context.Background()); err != nil {
		t.Fatalf("send() error: %v", err)
	}

	if got := gotHeader.Get("App-Key"); got != aptabaseAppKey {
		t.Errorf("App-Key = %q, want %q", got, aptabaseAppKey)
	}
	if got := gotHeader.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if len(gotEvents) != 1 {
		t.Fatalf("got %d events, want 1", len(gotEvents))
	}
	ev := gotEvents[0]
	if ev.EventName != "version_check" {
		t.Errorf("eventName = %q", ev.EventName)
	}
	if want := telemetrySessionID("alice", "ucat_secret"); ev.SessionID != want {
		t.Errorf("sessionId = %q, want %q", ev.SessionID, want)
	}
	if ev.SystemProps.AppVersion != "1.5.0-rc.1" {
		t.Errorf("appVersion = %q", ev.SystemProps.AppVersion)
	}
	if ev.SystemProps.OSName != osName(runtime.GOOS) {
		t.Errorf("osName = %q", ev.SystemProps.OSName)
	}
	for k, want := range map[string]any{"channel": "beta", "arch": runtime.GOARCH, "zone": "fi-hel1", "plan": "4xCPU-8GB"} {
		if got := ev.Props[k]; got != want {
			t.Errorf("props[%q] = %v, want %v", k, got, want)
		}
	}
}

func TestTelemetrySend_PasswordAuthKeysWithPassword(t *testing.T) {
	g := &InstanceGroup{Username: "alice", Password: "hunter2", log: hclog.NewNullLogger()}
	if got, want := g.newTelemetry("alice").sessionID, telemetrySessionID("alice", "hunter2"); got != want {
		t.Errorf("sessionID = %q, want %q", got, want)
	}
}

func TestTelemetrySend_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	orig := telemetryURL
	telemetryURL = srv.URL
	t.Cleanup(func() { telemetryURL = orig })

	g := &InstanceGroup{Token: "tok", log: hclog.NewNullLogger()}
	if err := g.newTelemetry("alice").send(context.Background()); err == nil {
		t.Fatal("send() expected error for a 401, got nil")
	}
}

func TestTelemetryRun_StopsOnCancel(t *testing.T) {
	pings := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case pings <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	orig := telemetryURL
	telemetryURL = srv.URL
	t.Cleanup(func() { telemetryURL = orig })

	g := &InstanceGroup{Token: "tok", log: hclog.NewNullLogger()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.newTelemetry("alice").run(ctx)
		close(done)
	}()

	<-pings // the first ping is sent immediately
	cancel()
	<-done
}
