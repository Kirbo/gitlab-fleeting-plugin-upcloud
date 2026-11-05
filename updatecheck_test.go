package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/go-hclog"
)

// releasesJSON is a GitLab releases API response, newest first. v1.4.1 is a
// patch released after v1.5.0, and v1.7.0 isn't released yet.
const releasesJSON = `[
	{"tag_name": "v1.7.0", "upcoming_release": true, "_links": {"self": "https://example.test/v1.7.0"}},
	{"tag_name": "v1.6.0-rc.1", "_links": {"self": "https://example.test/v1.6.0-rc.1"}},
	{"tag_name": "v1.4.1", "_links": {"self": "https://example.test/v1.4.1"}},
	{"tag_name": "v1.5.0", "_links": {"self": "https://example.test/v1.5.0"}},
	{"tag_name": "not-a-version"},
	{"tag_name": "v1.4.0", "_links": {"self": "https://example.test/v1.4.0"}}
]`

// ntfyPost is one request the fake ntfy server received.
type ntfyPost struct {
	header http.Header
	body   string
}

// fakeNtfy records posts and answers with status.
type fakeNtfy struct {
	mu     sync.Mutex
	status int
	posts  []ntfyPost
}

func (f *fakeNtfy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, ntfyPost{header: r.Header.Clone(), body: string(body)})
	w.WriteHeader(f.status)
}

func (f *fakeNtfy) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

// newTestChecker returns a checker for version that reads releases (a JSON
// response body) and posts to ntfy, if non-nil.
func newTestChecker(t *testing.T, version, releases string, ntfy *fakeNtfy) *updateChecker {
	t.Helper()
	setVersion(t, version)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, releases)
	}))
	t.Cleanup(api.Close)
	orig := releasesAPIURL
	releasesAPIURL = api.URL
	t.Cleanup(func() { releasesAPIURL = orig })

	g := &InstanceGroup{Name: "ci", log: hclog.NewNullLogger()}
	if ntfy != nil {
		srv := httptest.NewServer(ntfy)
		t.Cleanup(srv.Close)
		g.UpdateNotifyURL = srv.URL + "/my-topic"
		g.UpdateNotifyToken = "tk_secret"
	}
	return g.newUpdateChecker()
}

func TestUpdateCheckEnabled(t *testing.T) {
	off, on := false, true
	tests := []struct {
		name        string
		version     string
		updateCheck *bool
		want        bool
	}{
		{"default on", "1.4.0", nil, true},
		{"v-prefixed", "v1.4.0", nil, true},
		{"prerelease", "1.5.0-rc.1", nil, true},
		{"explicitly on", "1.4.0", &on, true},
		{"opted out", "1.4.0", &off, false},
		{"dev build", "dev", nil, false},
		{"commit build", "e14522a", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setVersion(t, tt.version)
			g := &InstanceGroup{UpdateCheck: tt.updateCheck}
			if got := g.updateCheckEnabled(); got != tt.want {
				t.Errorf("updateCheckEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateUpdateNotify(t *testing.T) {
	off := false
	tests := []struct {
		name    string
		group   *InstanceGroup
		wantErr string
	}{
		{"nothing set", &InstanceGroup{}, ""},
		{"own topic", &InstanceGroup{UpdateNotifyURL: "https://ntfy.sh/my-topic"}, ""},
		{"self-hosted with token", &InstanceGroup{UpdateNotifyURL: "http://ntfy.internal:8080/runners", UpdateNotifyToken: "tk_x"}, ""},
		{"token without url", &InstanceGroup{UpdateNotifyToken: "tk_x"}, "requires update_notify_url"},
		{"check disabled", &InstanceGroup{UpdateCheck: &off, UpdateNotifyURL: "https://ntfy.sh/my-topic"}, "requires update_check"},
		{"not a url", &InstanceGroup{UpdateNotifyURL: "my-topic"}, "http:// or https://"},
		{"no host", &InstanceGroup{UpdateNotifyURL: "https:///my-topic"}, "http:// or https://"},
		{"other scheme", &InstanceGroup{UpdateNotifyURL: "ftp://ntfy.sh/my-topic"}, "http:// or https://"},
		{"release channel", &InstanceGroup{UpdateNotifyURL: releaseNtfyURL}, "release channel"},
		{"release channel with slash", &InstanceGroup{UpdateNotifyURL: releaseNtfyURL + "/"}, "release channel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.group.validateUpdateNotify()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLatestRelease(t *testing.T) {
	tests := []struct {
		name, version, wantVersion, wantURL string
	}{
		// Highest by version, not the newest by date (v1.4.1); never upcoming
		// releases or prereleases for a stable build.
		{"stable build", "1.4.0", "v1.5.0", "https://example.test/v1.5.0"},
		// A prerelease build also hears about prereleases.
		{"beta build", "1.5.0-rc.2", "v1.6.0-rc.1", "https://example.test/v1.6.0-rc.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newTestChecker(t, tt.version, releasesJSON, nil)
			gotVersion, gotURL, err := u.latestRelease(context.Background())
			if err != nil {
				t.Fatalf("latestRelease() error: %v", err)
			}
			if gotVersion != tt.wantVersion || gotURL != tt.wantURL {
				t.Errorf("latestRelease() = %q, %q; want %q, %q", gotVersion, gotURL, tt.wantVersion, tt.wantURL)
			}
		})
	}
}

func TestLatestRelease_FallbackURL(t *testing.T) {
	u := newTestChecker(t, "1.4.0", `[{"tag_name": "v1.5.0"}]`, nil)
	_, gotURL, err := u.latestRelease(context.Background())
	if err != nil {
		t.Fatalf("latestRelease() error: %v", err)
	}
	if want := releasesPageURL + "/v1.5.0"; gotURL != want {
		t.Errorf("url = %q, want %q", gotURL, want)
	}
}

func TestLatestRelease_Errors(t *testing.T) {
	for name, body := range map[string]string{
		"no releases":      `[]`,
		"only prereleases": `[{"tag_name": "v1.6.0-rc.1"}]`,
		"malformed":        `{"message": "404 Project Not Found"}`,
		"truncated":        `[{"tag_name": `,
	} {
		t.Run(name, func(t *testing.T) {
			u := newTestChecker(t, "1.4.0", body, nil)
			if _, _, err := u.latestRelease(context.Background()); err == nil {
				t.Error("latestRelease() expected an error, got nil")
			}
		})
	}
}

func TestLatestRelease_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	u := newTestChecker(t, "1.4.0", releasesJSON, nil)
	u.releasesURL = srv.URL
	if _, _, err := u.latestRelease(context.Background()); err == nil {
		t.Error("latestRelease() expected an error for a 429, got nil")
	}
}

func TestCheck_NotifiesOncePerVersion(t *testing.T) {
	ntfy := &fakeNtfy{status: http.StatusOK}
	u := newTestChecker(t, "1.4.0", releasesJSON, ntfy)

	u.check(context.Background())
	u.check(context.Background())
	if got := ntfy.count(); got != 1 {
		t.Fatalf("got %d posts after two checks, want 1", got)
	}
	p := ntfy.posts[0]
	for k, want := range map[string]string{
		"Title":         "fleeting-plugin-upcloud v1.5.0 available",
		"Click":         "https://example.test/v1.5.0",
		"Authorization": "Bearer tk_secret",
	} {
		if got := p.header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, want := range []string{"v1.5.0 is available", `"ci"`, "running v1.4.0", "https://example.test/v1.5.0"} {
		if !strings.Contains(p.body, want) {
			t.Errorf("body %q doesn't contain %q", p.body, want)
		}
	}

	// A newer release gets its own post.
	newer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"tag_name": "v1.6.0"}]`)
	}))
	defer newer.Close()
	u.releasesURL = newer.URL
	u.check(context.Background())
	u.check(context.Background())
	if got := ntfy.count(); got != 2 {
		t.Fatalf("got %d posts after a newer release, want 2", got)
	}
	if got := ntfy.posts[1].header.Get("Title"); got != "fleeting-plugin-upcloud v1.6.0 available" {
		t.Errorf("Title = %q", got)
	}
}

func TestCheck_RetriesFailedPost(t *testing.T) {
	ntfy := &fakeNtfy{status: http.StatusTooManyRequests}
	u := newTestChecker(t, "1.4.0", releasesJSON, ntfy)

	u.check(context.Background())
	ntfy.mu.Lock()
	ntfy.status = http.StatusOK
	ntfy.mu.Unlock()
	u.check(context.Background())
	u.check(context.Background())
	if got := ntfy.count(); got != 2 {
		t.Errorf("got %d posts, want 2 (a failed one, then one retry)", got)
	}
	if u.notified != "v1.5.0" {
		t.Errorf("notified = %q, want v1.5.0", u.notified)
	}
}

func TestCheck_UpToDate(t *testing.T) {
	ntfy := &fakeNtfy{status: http.StatusOK}
	u := newTestChecker(t, "1.5.0", releasesJSON, ntfy)
	u.check(context.Background())
	if got := ntfy.count(); got != 0 {
		t.Errorf("got %d posts while up to date, want 0", got)
	}
	if u.announced != "" {
		t.Errorf("announced = %q while up to date", u.announced)
	}
}

func TestCheck_NoNotifyURL(t *testing.T) {
	u := newTestChecker(t, "1.4.0", releasesJSON, nil)
	u.check(context.Background())
	if u.announced != "v1.5.0" {
		t.Errorf("announced = %q, want v1.5.0", u.announced)
	}
	if u.notified != "" {
		t.Errorf("notified = %q without a notify URL", u.notified)
	}
}

func TestUpdateCheckRun_StopsOnCancel(t *testing.T) {
	checks := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case checks <- struct{}{}:
		default:
		}
		_, _ = io.WriteString(w, releasesJSON)
	}))
	defer srv.Close()
	u := newTestChecker(t, "1.4.0", releasesJSON, nil)
	u.releasesURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		u.run(ctx)
		close(done)
	}()

	<-checks // the first check runs immediately
	cancel()
	<-done
}

func TestValidate_ChecksUpdateNotify(t *testing.T) {
	g := baseGroup(newMockSvc())
	g.UpdateNotifyURL = releaseNtfyURL
	if err := g.validate(); err == nil || !strings.Contains(err.Error(), "release channel") {
		t.Errorf("validate() = %v, want the release channel error", err)
	}
}
