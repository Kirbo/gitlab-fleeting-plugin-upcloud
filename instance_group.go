package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	upcloud "github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/client"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/service"
	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"golang.org/x/crypto/ssh"
)

// upcloudSvc is the subset of the UpCloud API used by InstanceGroup.
// *service.Service satisfies this interface; tests substitute a mock.
type upcloudSvc interface {
	GetAccount(ctx context.Context) (*upcloud.Account, error)
	GetStorageDetails(ctx context.Context, r *request.GetStorageDetailsRequest) (*upcloud.StorageDetails, error)
	GetServersWithFilters(ctx context.Context, r *request.GetServersWithFiltersRequest) (*upcloud.Servers, error)
	CreateServer(ctx context.Context, r *request.CreateServerRequest) (*upcloud.ServerDetails, error)
	StopServer(ctx context.Context, r *request.StopServerRequest) (*upcloud.ServerDetails, error)
	WaitForServerState(ctx context.Context, r *request.WaitForServerStateRequest) (*upcloud.ServerDetails, error)
	DeleteServerAndStorages(ctx context.Context, r *request.DeleteServerAndStoragesRequest) error
	GetServerDetails(ctx context.Context, r *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error)
}

// newUpcloudService constructs the production UpCloud service. Tests may replace this.
var newUpcloudService = func(c *client.Client) upcloudSvc {
	return service.New(c)
}

const (
	groupLabelKey      = "fleeting-group"
	defaultPlan        = "1xCPU-2GB"
	defaultNamePrefix  = "fleeting"
	defaultTitlePrefix = "fleeting-plugin-upcloud"
	defaultMaxSize     = 100

	// maxConcurrentRequests bounds parallel UpCloud API calls in Increase,
	// Decrease, background deletions and Shutdown so a large scale event
	// doesn't trip API rate limits.
	maxConcurrentRequests = 10

	// serverStopTimeout bounds how long Shutdown waits for a server stopped by
	// Decrease to reach the stopped state. The caller's ctx may have no
	// deadline, and the SDK's WaitForServerState polls until its ctx is done.
	serverStopTimeout = 5 * time.Minute

	// deleteRetryInterval throttles background deletions of the same server,
	// so a persistently failing delete is retried without hammering the API.
	// It exceeds the 30s client timeout, so attempts never overlap.
	deleteRetryInterval = time.Minute
)

// InstanceGroup implements provider.InstanceGroup for UpCloud.
// Fields are populated from [runners.autoscaler.plugin_config] in config.toml.
type InstanceGroup struct {
	// Auth config: set either Token OR Username+Password
	Token    string `json:"token"`    // UpCloud Personal Access Token (ucat_...)
	Username string `json:"username"` // UpCloud API username (mutually exclusive with Token)
	Password string `json:"password"` // UpCloud API password (mutually exclusive with Token)

	// Required config
	Zone     string `json:"zone"`
	Template string `json:"template"`
	Name     string `json:"name"` // unique group name; used as UpCloud label value

	// Optional config
	Plan              string `json:"plan"`                // default: "1xCPU-2GB"
	StorageSize       int    `json:"storage_size"`        // GB, 0 = inherit from template
	StorageTier       string `json:"storage_tier"`        // "maxiops", "standard" or "hdd"; default: inherit from template
	NamePrefix        string `json:"name_prefix"`         // hostname prefix, default: "fleeting"
	TitlePrefix       string `json:"title_prefix"`        // server title prefix, default: "fleeting-plugin-upcloud"
	MaxSize           int    `json:"max_size"`            // default: 100
	Network           string `json:"network"`             // private network UUID to attach; required by use_private_network
	UsePrivateNetwork bool   `json:"use_private_network"` // default: false (use public IP)
	UserData          string `json:"user_data"`           // optional: URL or script body for server initialization

	// Internal state
	log       hclog.Logger
	settings  provider.Settings
	svc       upcloudSvc
	publicKey string // SSH authorized_keys format, derived from settings.ConnectorConfig.Key

	// Background deletions outlive the Update call that starts them, so they
	// run on a plugin-lifetime context that Shutdown cancels.
	ctx        context.Context
	cancel     context.CancelFunc
	cleanups   sync.WaitGroup
	cleanupSem chan struct{}

	mu sync.Mutex
	// deleteAttempts records when a background delete of each stopped or
	// errored server last started; see deleteInBackground.
	deleteAttempts map[string]time.Time
	// stopping holds servers Decrease hard-stopped whose deletion hasn't
	// been confirmed yet; Shutdown finishes them.
	stopping map[string]struct{}
}

// validate checks that required config fields are set and applies defaults.
func (g *InstanceGroup) validate() error {
	if g.Token == "" && (g.Username == "" || g.Password == "") {
		return fmt.Errorf("either token or both username and password are required")
	}
	if g.Zone == "" {
		return fmt.Errorf("zone is required")
	}
	if g.Template == "" {
		return fmt.Errorf("template is required")
	}
	if g.Name == "" {
		return fmt.Errorf("name is required")
	}
	if g.UsePrivateNetwork && g.Network == "" {
		return fmt.Errorf("network (a private network UUID) is required when use_private_network is true")
	}
	switch g.StorageTier {
	case "", upcloud.StorageTierMaxIOPS, upcloud.StorageTierStandard, upcloud.StorageTierHDD:
	default:
		return fmt.Errorf("storage_tier must be one of %q, %q or %q, got %q",
			upcloud.StorageTierMaxIOPS, upcloud.StorageTierStandard, upcloud.StorageTierHDD, g.StorageTier)
	}
	if g.StorageSize < 0 {
		return fmt.Errorf("storage_size must not be negative")
	}
	if g.MaxSize < 0 {
		return fmt.Errorf("max_size must not be negative")
	}
	if g.Plan == "" {
		g.Plan = defaultPlan
	}
	if g.NamePrefix == "" {
		g.NamePrefix = defaultNamePrefix
	}
	// UpCloud rejects hostnames that aren't RFC 1123 labels (uppercase letters,
	// underscores, etc. trigger HOSTNAME_INVALID). Sanitize the configured prefix
	// so values like "CI" or "My_Runner" still work, falling back to the default
	// if nothing valid remains.
	if g.NamePrefix = sanitizeNamePrefix(g.NamePrefix); g.NamePrefix == "" {
		g.NamePrefix = defaultNamePrefix
	}
	if g.TitlePrefix == "" {
		g.TitlePrefix = defaultTitlePrefix
	}
	// UpCloud caps server titles at 255 characters; truncate the prefix so
	// "<prefix> - <hostname>" always fits instead of failing every create.
	if len(g.TitlePrefix) > maxTitlePrefixLen {
		g.TitlePrefix = strings.TrimRight(g.TitlePrefix[:maxTitlePrefixLen], " ")
	}
	if g.MaxSize == 0 {
		g.MaxSize = defaultMaxSize
	}
	return nil
}

// newClient creates an authenticated UpCloud API client.
// Uses bearer token auth if Token is set, otherwise Basic Auth.
func (g *InstanceGroup) newClient() *client.Client {
	if g.Token != "" {
		return client.New("", "", client.WithBearerAuth(g.Token), client.WithTimeout(30*time.Second))
	}
	return client.New(g.Username, g.Password, client.WithTimeout(30*time.Second))
}

// initState prepares the internal state used by background deletions.
func (g *InstanceGroup) initState() {
	g.ctx, g.cancel = context.WithCancel(context.Background())
	g.cleanupSem = make(chan struct{}, maxConcurrentRequests)
	g.deleteAttempts = make(map[string]time.Time)
	g.stopping = make(map[string]struct{})
}

// Init is called once at startup. It validates config, derives the SSH public key,
// creates the UpCloud client, and validates credentials and the template.
func (g *InstanceGroup) Init(ctx context.Context, log hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	g.log = log
	g.settings = settings

	if err := g.validate(); err != nil {
		return provider.ProviderInfo{}, err
	}

	// Derive SSH public key from the private key provided via connector_config.key_path
	if len(settings.Key) > 0 {
		signer, err := ssh.ParsePrivateKey(settings.Key)
		if err != nil {
			return provider.ProviderInfo{}, fmt.Errorf("parsing SSH private key from connector_config: %w", err)
		}
		g.publicKey = string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	} else {
		log.Warn("no SSH key configured in connector_config.key_path; instances will be created without SSH key injection")
	}

	g.svc = newUpcloudService(g.newClient())

	// Validate credentials
	if _, err := g.svc.GetAccount(ctx); err != nil {
		return provider.ProviderInfo{}, fmt.Errorf("authenticating with UpCloud API: %w", err)
	}

	// Fail fast on a mistyped template UUID instead of failing every create.
	// Other errors (e.g. a subaccount without storage permissions) only warn:
	// creates may still succeed.
	if _, err := g.svc.GetStorageDetails(ctx, &request.GetStorageDetailsRequest{UUID: g.Template}); err != nil {
		if isNotFound(err) {
			return provider.ProviderInfo{}, fmt.Errorf("template %s not found: %w", g.Template, err)
		}
		log.Warn("could not verify template", "template", g.Template, "error", err)
	}

	g.initState()

	log.Info("initialized", "zone", g.Zone, "group", g.Name, "plan", g.Plan)

	return provider.ProviderInfo{
		ID:        fmt.Sprintf("upcloud/%s/%s", g.Zone, g.Name),
		MaxSize:   g.MaxSize,
		Version:   Version.Version,
		BuildInfo: fmt.Sprintf("%s@%s built %s", Version.Name, Version.Revision, Version.BuiltAt),
	}, nil
}

// Update polls UpCloud for the current state of all instances in this group,
// calling fn for each discovered instance. Stopped and errored servers are
// deleted in the background, since nothing else removes them (see
// mapServerState).
func (g *InstanceGroup) Update(ctx context.Context, fn func(instance string, state provider.State)) error {
	servers, err := g.svc.GetServersWithFilters(ctx, &request.GetServersWithFiltersRequest{
		Filters: []request.QueryFilter{
			request.FilterLabel{Label: upcloud.Label{Key: groupLabelKey, Value: g.Name}},
		},
	})
	if err != nil {
		return fmt.Errorf("listing group servers: %w", err)
	}

	listed := make(map[string]struct{}, len(servers.Servers))
	for _, s := range servers.Servers {
		listed[s.UUID] = struct{}{}
		state := mapServerState(s.State)
		if state == provider.StateDeleting {
			g.deleteInBackground(s.UUID)
		}
		fn(s.UUID, state)
	}

	// Forget servers that are gone so the bookkeeping maps don't grow forever.
	g.mu.Lock()
	for uuid := range g.deleteAttempts {
		if _, ok := listed[uuid]; !ok {
			delete(g.deleteAttempts, uuid)
		}
	}
	for uuid := range g.stopping {
		if _, ok := listed[uuid]; !ok {
			delete(g.stopping, uuid)
		}
	}
	g.mu.Unlock()

	return nil
}

// mapServerState converts an UpCloud server state string to a provider.State.
// Stopped and errored servers still exist (and bill for storage) but can't run
// jobs, so they are reported as StateDeleting. Fleeting never passes
// StateDeleting instances to Decrease, so Update deletes them itself. This
// covers servers Decrease hard-stopped, servers whose earlier delete failed,
// and servers stopped outside the plugin.
func mapServerState(s string) provider.State {
	switch s {
	case upcloud.ServerStateStarted:
		return provider.StateRunning
	case upcloud.ServerStateStopped, upcloud.ServerStateError:
		return provider.StateDeleting
	default:
		// "new", "maintenance", etc.
		return provider.StateCreating
	}
}

// deleteInBackground deletes a stopped or errored server without blocking
// Update. Attempts per server are throttled to one per deleteRetryInterval.
func (g *InstanceGroup) deleteInBackground(uuid string) {
	g.mu.Lock()
	if last, ok := g.deleteAttempts[uuid]; ok && time.Since(last) < deleteRetryInterval {
		g.mu.Unlock()
		return
	}
	g.deleteAttempts[uuid] = time.Now()
	g.mu.Unlock()

	g.cleanups.Add(1)
	go func() {
		defer g.cleanups.Done()
		g.cleanupSem <- struct{}{}
		defer func() { <-g.cleanupSem }()

		if err := g.deleteServer(g.ctx, uuid); err != nil {
			g.log.Error("failed to delete stopped instance; will retry", "uuid", uuid, "error", err)
			return
		}
		g.mu.Lock()
		delete(g.stopping, uuid)
		g.mu.Unlock()
	}()
}

// Increase creates n new UpCloud servers in this group in parallel (bounded
// by maxConcurrentRequests). It returns the number of servers successfully
// requested and an error aggregating any failed creations, so the taskscaler
// can back off instead of hot-looping on a persistently failing config.
func (g *InstanceGroup) Increase(ctx context.Context, n int) (int, error) {
	var (
		mu        sync.Mutex
		succeeded int
		errs      []error
		wg        sync.WaitGroup
	)
	sem := make(chan struct{}, maxConcurrentRequests)

	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Don't start new creates once the caller has given up.
			if err := ctx.Err(); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}

			err := g.createServer(ctx)
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
			} else {
				succeeded++
			}
			mu.Unlock()
		}()
	}

	wg.Wait()
	return succeeded, errors.Join(errs...)
}

// createServer provisions a single server in this group.
func (g *InstanceGroup) createServer(ctx context.Context) error {
	hostname := fmt.Sprintf("%s-%s", g.NamePrefix, randomSuffix(8))

	storageDevices := request.CreateServerStorageDeviceSlice{
		{
			Action:  request.CreateServerStorageDeviceActionClone,
			Storage: g.Template,
			Title:   "disk1",
			Size:    g.StorageSize,
			Tier:    g.StorageTier, // empty = inherit tier from template
		},
	}

	networking := &request.CreateServerNetworking{
		Interfaces: request.CreateServerInterfaceSlice{
			{
				IPAddresses: request.CreateServerIPAddressSlice{
					{Family: upcloud.IPAddressFamilyIPv4},
				},
				Type: upcloud.NetworkTypePublic,
			},
		},
	}

	// UpCloud requires the network UUID for private interfaces.
	if g.Network != "" {
		networking.Interfaces = append(networking.Interfaces, request.CreateServerInterface{
			IPAddresses: request.CreateServerIPAddressSlice{
				{Family: upcloud.IPAddressFamilyIPv4},
			},
			Type:    upcloud.NetworkTypePrivate,
			Network: g.Network,
		})
	}

	createReq := &request.CreateServerRequest{
		Hostname: hostname,
		Title:    fmt.Sprintf("%s - %s", g.TitlePrefix, hostname),
		Plan:     g.Plan,
		Zone:     g.Zone,
		Metadata: upcloud.True,
		Labels: &upcloud.LabelSlice{
			{Key: groupLabelKey, Value: g.Name},
		},
		StorageDevices: storageDevices,
		Networking:     networking,
	}

	if g.publicKey != "" {
		createReq.LoginUser = &request.LoginUser{
			Username: g.settings.Username,
			SSHKeys:  request.SSHKeySlice{g.publicKey},
		}
	}

	if g.UserData != "" {
		createReq.UserData = g.UserData
	}

	if _, err := g.svc.CreateServer(ctx, createReq); err != nil {
		g.log.Error("failed to create server", "hostname", hostname, "error", err)
		return fmt.Errorf("creating server %s: %w", hostname, err)
	}

	g.log.Info("created server", "hostname", hostname)
	return nil
}

// Decrease starts removing the specified instances in parallel (bounded by
// maxConcurrentRequests). It returns the UUIDs of instances whose removal was
// successfully requested and an error aggregating any failures.
func (g *InstanceGroup) Decrease(ctx context.Context, instances []string) ([]string, error) {
	return forEachConcurrently(instances, func(uuid string) error {
		if err := g.removeInstance(ctx, uuid); err != nil {
			g.log.Error("failed to remove instance", "uuid", uuid, "error", err)
			return err
		}
		return nil
	})
}

// removeInstance starts removing a server without waiting for it to stop.
// Fleeting calls Decrease and Update from the same loop, so waiting here would
// stall provisioning. Running servers are hard-stopped and deleted by Update
// once UpCloud reports them stopped. Stopped or errored servers (a stop
// attempt would fail) are deleted directly, and missing ones count as removed.
func (g *InstanceGroup) removeInstance(ctx context.Context, uuid string) error {
	details, err := g.svc.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: uuid})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting server details for %s: %w", uuid, err)
	}

	switch details.State {
	case upcloud.ServerStateStopped, upcloud.ServerStateError:
		return g.deleteServer(ctx, uuid)
	}

	if _, err := g.svc.StopServer(ctx, &request.StopServerRequest{
		UUID:     uuid,
		StopType: request.ServerStopTypeHard,
	}); err != nil {
		return fmt.Errorf("stopping server %s: %w", uuid, err)
	}

	g.mu.Lock()
	g.stopping[uuid] = struct{}{}
	g.mu.Unlock()

	g.log.Info("stopping instance for removal", "uuid", uuid)
	return nil
}

// deleteServer deletes a stopped server along with all its storage devices.
// A server that is already gone counts as deleted.
func (g *InstanceGroup) deleteServer(ctx context.Context, uuid string) error {
	err := g.svc.DeleteServerAndStorages(ctx, &request.DeleteServerAndStoragesRequest{UUID: uuid})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("deleting server %s: %w", uuid, err)
	}
	g.log.Info("removed instance", "uuid", uuid)
	return nil
}

// ConnectInfo returns connection details for a specific instance.
func (g *InstanceGroup) ConnectInfo(ctx context.Context, id string) (provider.ConnectInfo, error) {
	// Start with defaults from runner's connector_config (includes key, username, protocol, etc.)
	info := provider.ConnectInfo{ConnectorConfig: g.settings.ConnectorConfig}
	info.ID = id

	details, err := g.svc.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: id})
	if err != nil {
		return info, fmt.Errorf("getting server details for %s: %w", id, err)
	}

	// Apply defaults only if not already set by the runner's connector_config
	if info.OS == "" {
		info.OS = "linux"
	}
	if info.Arch == "" {
		info.Arch = "amd64"
	}
	if info.Protocol == "" {
		info.Protocol = provider.ProtocolSSH
	}

	// Extract IPv4 addresses
	for _, ip := range details.IPAddresses {
		if ip.Family != upcloud.IPAddressFamilyIPv4 {
			continue
		}
		switch ip.Access {
		case upcloud.IPAddressAccessPublic:
			info.ExternalAddr = ip.Address
		case upcloud.IPAddressAccessPrivate:
			info.InternalAddr = ip.Address
		}
	}

	if g.UsePrivateNetwork {
		if info.InternalAddr != "" {
			info.ExternalAddr = info.InternalAddr
		} else {
			g.log.Warn("use_private_network is set but instance has no private IPv4 address; falling back to public address", "uuid", id)
		}
	}

	return info, nil
}

// Heartbeat checks whether a specific instance is still healthy.
func (g *InstanceGroup) Heartbeat(ctx context.Context, id string) error {
	details, err := g.svc.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: id})
	if err != nil {
		// A 404 is definitive: the server no longer exists and must be replaced.
		if isNotFound(err) {
			return fmt.Errorf("server %s not found: %w", id, err)
		}
		// Treat other (transient) API errors as healthy to avoid premature instance replacement
		g.log.Warn("heartbeat API error (treating as healthy)", "uuid", id, "error", err)
		return nil
	}

	switch details.State {
	case upcloud.ServerStateError, upcloud.ServerStateStopped:
		return fmt.Errorf("server %s is %s", id, details.State)
	}

	return nil
}

// Suspend is not supported: UpCloud has no suspend-to-disk primitive that
// fits fleeting's suspend/resume lifecycle, and the plugin does not declare
// provider.CapabilitySuspendResume.
func (g *InstanceGroup) Suspend(_ context.Context, _ []string) ([]string, error) {
	return nil, provider.ErrCapabilityNotSupported
}

// Resume is not supported; see Suspend.
func (g *InstanceGroup) Resume(_ context.Context, _ []string) ([]string, error) {
	return nil, provider.ErrCapabilityNotSupported
}

// Shutdown finishes removals still in progress, so servers stopped by
// Decrease aren't left behind (billing for storage) when the runner exits,
// e.g. after delete_instances_on_shutdown. It waits for background deletions,
// then for each server Decrease stopped to reach the stopped state before
// deleting it. ctx bounds the whole wait.
func (g *InstanceGroup) Shutdown(ctx context.Context) error {
	if g.cancel == nil {
		return nil // Init never completed
	}
	defer g.cancel()

	done := make(chan struct{})
	go func() {
		g.cleanups.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("waiting for background deletions: %w", ctx.Err())
	}

	g.mu.Lock()
	pending := make([]string, 0, len(g.stopping))
	for uuid := range g.stopping {
		pending = append(pending, uuid)
	}
	g.mu.Unlock()

	_, err := forEachConcurrently(pending, func(uuid string) error {
		return g.finishRemoval(ctx, uuid)
	})
	return err
}

// finishRemoval waits for a server Decrease stopped to reach the stopped state,
// then deletes it.
func (g *InstanceGroup) finishRemoval(ctx context.Context, uuid string) error {
	details, err := g.svc.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: uuid})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting server details for %s: %w", uuid, err)
	}

	switch details.State {
	case upcloud.ServerStateStopped, upcloud.ServerStateError:
	default:
		waitCtx, cancel := context.WithTimeout(ctx, serverStopTimeout)
		defer cancel()

		if _, err := g.svc.WaitForServerState(waitCtx, &request.WaitForServerStateRequest{
			UUID:         uuid,
			DesiredState: upcloud.ServerStateStopped,
		}); err != nil {
			if isNotFound(err) {
				return nil
			}
			return fmt.Errorf("waiting for server %s to stop: %w", uuid, err)
		}
	}

	return g.deleteServer(ctx, uuid)
}

// forEachConcurrently calls fn for each id in parallel, at most
// maxConcurrentRequests at a time. It returns the ids for which fn succeeded
// and the joined errors of the rest.
func forEachConcurrently(ids []string, fn func(id string) error) ([]string, error) {
	var (
		mu        sync.Mutex
		succeeded []string
		errs      []error
		wg        sync.WaitGroup
	)
	sem := make(chan struct{}, maxConcurrentRequests)

	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			err := fn(id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			succeeded = append(succeeded, id)
		}()
	}

	wg.Wait()
	return succeeded, errors.Join(errs...)
}

// isNotFound reports whether err is an UpCloud API 404 response.
func isNotFound(err error) bool {
	var problem *upcloud.Problem
	return errors.As(err, &problem) && problem.Status == http.StatusNotFound
}

// namePrefixInvalidChars matches runs of characters not allowed in a hostname label.
var namePrefixInvalidChars = regexp.MustCompile(`[^a-z0-9]+`)

// maxNamePrefixLen keeps "<prefix>-<8 char suffix>" within the 63-character
// RFC 1123 hostname label limit.
const maxNamePrefixLen = 63 - 1 - 8

// maxTitlePrefixLen keeps "<prefix> - <hostname>" within UpCloud's
// 255-character server title limit (hostname ≤ 63, separator = 3).
const maxTitlePrefixLen = 255 - 3 - 63

// sanitizeNamePrefix normalizes an arbitrary name_prefix into a valid hostname
// label fragment: lowercased, with runs of invalid characters collapsed to a
// single hyphen, leading/trailing hyphens trimmed, and truncated so the full
// hostname stays within the 63-character label limit. UpCloud rejects hostnames
// that aren't RFC 1123 labels, so "CI" becomes "ci" and "My_Runner!" becomes
// "my-runner". Returns "" if nothing valid remains.
func sanitizeNamePrefix(s string) string {
	s = strings.ToLower(s)
	s = namePrefixInvalidChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > maxNamePrefixLen {
		s = strings.TrimRight(s[:maxNamePrefixLen], "-")
	}
	return s
}

// randomSuffix generates a random lowercase alphanumeric string of length n.
// It only needs to make hostnames unique, so math/rand is sufficient.
func randomSuffix(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))] //nolint:gosec // not security-sensitive
	}
	return string(b)
}
