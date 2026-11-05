package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
)

// aptabaseAppKey identifies this plugin's app on the self-hosted Aptabase
// instance (not a secret: it only allows sending events). Telemetry is not
// sent while it is empty. Tests replace it.
var aptabaseAppKey = "A-SH-3441623680"

const (
	// telemetryInterval is how often a running plugin pings. The badge
	// backend counts distinct installs over a rolling 26h window, so a steady
	// cadence keeps that count steady around the clock.
	telemetryInterval = 15 * time.Minute

	telemetryTimeout = 10 * time.Second
)

// telemetryURL is the Aptabase ingest endpoint. Tests replace it.
var telemetryURL = "https://aptabase.devaus.eu/api/v0/events"

// telemetry sends anonymous usage pings to Aptabase.
type telemetry struct {
	url       string
	appKey    string
	client    *http.Client
	log       hclog.Logger
	sessionID string
	version   string
	props     map[string]any
}

type aptabaseEvent struct {
	Timestamp   string              `json:"timestamp"`
	SessionID   string              `json:"sessionId"`
	EventName   string              `json:"eventName"`
	SystemProps aptabaseSystemProps `json:"systemProps"`
	Props       map[string]any      `json:"props"`
}

type aptabaseSystemProps struct {
	IsDebug    bool   `json:"isDebug"`
	AppVersion string `json:"appVersion"`
	OSName     string `json:"osName"`
	OSVersion  string `json:"osVersion"`
	SDKVersion string `json:"sdkVersion"`
}

// telemetryEnabled reports whether this build and config should send
// telemetry: on by default, off with `telemetry = false`, and never for
// development builds or when no App-Key is compiled in.
func (g *InstanceGroup) telemetryEnabled() bool {
	if g.Telemetry != nil && !*g.Telemetry {
		return false
	}
	return Version.Version != "dev" && aptabaseAppKey != ""
}

// newTelemetry builds the pinger for this group. accountUsername is the
// UpCloud account the credential belongs to.
func (g *InstanceGroup) newTelemetry(accountUsername string) *telemetry {
	key := g.Token
	if key == "" {
		key = g.Password
	}
	version := strings.TrimPrefix(Version.Version, "v")
	return &telemetry{
		url:       telemetryURL,
		appKey:    aptabaseAppKey,
		client:    &http.Client{Timeout: telemetryTimeout},
		log:       g.log,
		sessionID: telemetrySessionID(accountUsername, key),
		version:   version,
		props: map[string]any{
			"channel": releaseChannel(version),
			"arch":    runtime.GOARCH,
			"zone":    g.Zone,
			"plan":    g.Plan,
		},
	}
}

// telemetrySessionID derives a stable, GUID-shaped Aptabase session id for an
// UpCloud account. The badge counts distinct installs over a rolling window,
// which needs an id that survives restarts and UTC midnight (Aptabase's own
// user_id rotates daily). It is HMAC-SHA256 of the account username keyed
// with the API credential, first 16 bytes as 8-4-4-4-12:
//   - GUID-shaped, which Aptabase requires (it silently drops other ids);
//   - pseudonymous: a plain hash of the username could be confirmed by anyone
//     who knows it, but the key never leaves the runner manager.
//
// Rotating the token changes the id, so that account counts twice for up to
// one window afterwards.
func telemetrySessionID(username, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(username))
	h := hex.EncodeToString(mac.Sum(nil))[:32]
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// releaseChannel maps a version to the badge backend's channel: prereleases
// (e.g. 1.5.0-rc.1) are "beta", everything else "stable".
func releaseChannel(version string) string {
	if strings.Contains(version, "-") {
		return "beta"
	}
	return "stable"
}

// osName maps GOOS to the name Aptabase shows.
func osName(goos string) string {
	switch goos {
	case "linux":
		return "Linux"
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	default:
		return goos
	}
}

// run pings once immediately and then every telemetryInterval until ctx is
// cancelled.
func (t *telemetry) run(ctx context.Context) {
	ticker := time.NewTicker(telemetryInterval)
	defer ticker.Stop()
	for {
		if err := t.send(ctx); err != nil && ctx.Err() == nil {
			t.log.Debug("telemetry ping failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// send posts one version_check event.
func (t *telemetry) send(ctx context.Context) error {
	body, err := json.Marshal([]aptabaseEvent{{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		SessionID: t.sessionID,
		EventName: "version_check",
		SystemProps: aptabaseSystemProps{
			AppVersion: t.version,
			OSName:     osName(runtime.GOOS),
			SDKVersion: Version.Name + "@" + t.version,
		},
		Props: t.props,
	}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("App-Key", t.appKey)
	req.Header.Set("User-Agent", Version.Name+"/"+t.version)
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}
