package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/mod/semver"
)

// releaseNtfyURL is the project's public ntfy channel. The release pipeline
// (notify-release in .gitlab-ci.yml) posts each new release there once, for
// anyone to subscribe to. The plugin never posts to it: validate rejects it as
// update_notify_url, since every install posting there would flood it.
const releaseNtfyURL = "https://ntfy.sh/OKjnFezDgjw7JE6B"

const (
	// updateCheckInterval is how often a running plugin looks for a newer
	// release, after the check at startup.
	updateCheckInterval = time.Hour

	updateCheckTimeout = 10 * time.Second

	// maxReleasesBody bounds how much of the releases API response is read.
	maxReleasesBody = 4 << 20
)

// releasesAPIURL lists this project's GitLab releases, newest first. Tests
// replace it.
var releasesAPIURL = "https://gitlab.com/api/v4/projects/KirboDev%2Fagentic-coding%2Fgitlab-fleeting-plugin-upcloud/releases?per_page=50"

// releasesPageURL is where a release's page lives, when the API gives none.
const releasesPageURL = "https://gitlab.com/KirboDev/agentic-coding/gitlab-fleeting-plugin-upcloud/-/releases"

// updateChecker periodically compares the running version with the latest
// release on its channel, logs a warning when a newer one exists, and posts it
// to the user's own ntfy topic if one is configured. It does each at most once
// per version for the lifetime of the plugin process.
type updateChecker struct {
	releasesURL string
	notifyURL   string
	notifyToken string
	group       string
	current     string // running version, "v"-prefixed SemVer
	stableOnly  bool   // stable builds only hear about stable releases
	client      *http.Client
	log         hclog.Logger

	announced string // latest version already logged
	notified  string // latest version already posted to notifyURL
}

type gitlabRelease struct {
	TagName  string `json:"tag_name"`
	Upcoming bool   `json:"upcoming_release"`
	Links    struct {
		Self string `json:"self"`
	} `json:"_links"`
}

// semverVersion returns the build version as "v"-prefixed SemVer, or "" for
// builds that aren't releases (e.g. "dev", or a commit SHA in CI).
func semverVersion() string {
	v := "v" + strings.TrimPrefix(Version.Version, "v")
	if !semver.IsValid(v) {
		return ""
	}
	return v
}

// updateCheckEnabled reports whether this build and config should check for
// updates: on by default, off with `update_check = false`, and never for
// builds that aren't releases.
func (g *InstanceGroup) updateCheckEnabled() bool {
	if g.UpdateCheck != nil && !*g.UpdateCheck {
		return false
	}
	return semverVersion() != ""
}

// validateUpdateNotify checks the update_* options.
func (g *InstanceGroup) validateUpdateNotify() error {
	if g.UpdateNotifyToken != "" && g.UpdateNotifyURL == "" {
		return errors.New("update_notify_token requires update_notify_url")
	}
	if g.UpdateNotifyURL == "" {
		return nil
	}
	if g.UpdateCheck != nil && !*g.UpdateCheck {
		return errors.New("update_notify_url requires update_check, which is false")
	}
	u, err := url.Parse(g.UpdateNotifyURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("update_notify_url must be an http:// or https:// URL, got %q", g.UpdateNotifyURL)
	}
	if isReleaseChannel(u) {
		return fmt.Errorf("update_notify_url must be your own ntfy topic: %s is the project's release channel, which you can subscribe to directly", releaseNtfyURL)
	}
	return nil
}

// isReleaseChannel reports whether u publishes to the release channel in any
// spelling ntfy accepts: either scheme, any host case, a trailing dot, an
// explicit port, user info, a query, extra slashes, or a publish sub-path
// such as /<topic>/publish. Topics are case-sensitive, so the topic isn't.
func isReleaseChannel(u *url.URL) bool {
	channel, err := url.Parse(releaseNtfyURL)
	if err != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host != channel.Hostname() {
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg != "" {
			// The first path segment is the topic.
			return seg == strings.Trim(channel.Path, "/")
		}
	}
	return false
}

func (g *InstanceGroup) newUpdateChecker() *updateChecker {
	current := semverVersion()
	return &updateChecker{
		releasesURL: releasesAPIURL,
		notifyURL:   g.UpdateNotifyURL,
		notifyToken: g.UpdateNotifyToken,
		group:       g.Name,
		current:     current,
		stableOnly:  semver.Prerelease(current) == "",
		client:      &http.Client{Timeout: updateCheckTimeout},
		log:         g.log,
	}
}

// run checks once immediately and then every updateCheckInterval until ctx is
// cancelled.
func (u *updateChecker) run(ctx context.Context) {
	ticker := time.NewTicker(updateCheckInterval)
	defer ticker.Stop()
	for {
		u.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// check looks up the latest release and reports it if it's newer than the
// running version. Failures to look it up are only logged at debug level, like
// telemetry, so they never affect the runner.
func (u *updateChecker) check(ctx context.Context) {
	latest, releaseURL, err := u.latestRelease(ctx)
	if err != nil {
		if ctx.Err() == nil {
			u.log.Debug("update check failed", "error", err)
		}
		return
	}
	if semver.Compare(latest, u.current) <= 0 {
		u.log.Debug("running the latest release", "version", u.current)
		return
	}

	if u.announced != latest {
		u.log.Warn("a newer release of the plugin is available", "current", u.current, "latest", latest, "url", releaseURL)
		u.announced = latest
	}

	if u.notifyURL == "" || u.notified == latest {
		return
	}
	// A failed post is retried on the next check. It warns because the user
	// configured the topic and would otherwise miss the notification.
	if err := u.notify(ctx, latest, releaseURL); err != nil {
		if ctx.Err() == nil {
			u.log.Warn("posting the update notification failed", "error", err)
		}
		return
	}
	u.notified = latest
}

// latestRelease returns the highest released version on this build's channel
// and its release page. Releases are compared by version, not date, so a
// patch to an older series never hides a newer release.
func (u *updateChecker) latestRelease(ctx context.Context) (version, releaseURL string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.releasesURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", Version.Name+"/"+strings.TrimPrefix(u.current, "v"))
	resp, err := u.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", "", fmt.Errorf("unexpected status %s", resp.Status)
	}
	var releases []gitlabRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleasesBody)).Decode(&releases); err != nil {
		return "", "", fmt.Errorf("decoding releases: %w", err)
	}

	var best *gitlabRelease
	for i := range releases {
		r := &releases[i]
		if r.Upcoming || !semver.IsValid(r.TagName) {
			continue
		}
		if u.stableOnly && semver.Prerelease(r.TagName) != "" {
			continue
		}
		if best == nil || semver.Compare(r.TagName, best.TagName) > 0 {
			best = r
		}
	}
	if best == nil {
		return "", "", errors.New("no releases found")
	}
	releaseURL = best.Links.Self
	if releaseURL == "" {
		releaseURL = releasesPageURL + "/" + best.TagName
	}
	return best.TagName, releaseURL, nil
}

// notify posts the newer release to the user's ntfy topic.
func (u *updateChecker) notify(ctx context.Context, latest, releaseURL string) error {
	body := fmt.Sprintf("%s %s is available. Runner group %q is running %s.\n\nRelease notes: %s",
		Version.Name, latest, u.group, u.current, releaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.notifyURL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("User-Agent", Version.Name+"/"+strings.TrimPrefix(u.current, "v"))
	req.Header.Set("Title", Version.Name+" "+latest+" available")
	req.Header.Set("Tags", "arrow_up")
	req.Header.Set("Click", releaseURL)
	if u.notifyToken != "" {
		req.Header.Set("Authorization", "Bearer "+u.notifyToken)
	}
	resp, err := u.client.Do(req)
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
