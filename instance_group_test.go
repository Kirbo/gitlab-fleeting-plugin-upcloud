package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	upcloud "github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/client"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"golang.org/x/crypto/ssh"
)

// ─── mock ────────────────────────────────────────────────────────────────────

// mockSvc is a test double for upcloudSvc.
// Each field is a function so individual tests can customise behaviour.
// Calls are serialized, so the functions may update test state without locking.
type mockSvc struct {
	mu                      sync.Mutex
	getAccount              func(context.Context) (*upcloud.Account, error)
	getStorageDetails       func(context.Context, *request.GetStorageDetailsRequest) (*upcloud.StorageDetails, error)
	getServersWithFilters   func(context.Context, *request.GetServersWithFiltersRequest) (*upcloud.Servers, error)
	createServer            func(context.Context, *request.CreateServerRequest) (*upcloud.ServerDetails, error)
	stopServer              func(context.Context, *request.StopServerRequest) (*upcloud.ServerDetails, error)
	waitForServerState      func(context.Context, *request.WaitForServerStateRequest) (*upcloud.ServerDetails, error)
	deleteServerAndStorages func(context.Context, *request.DeleteServerAndStoragesRequest) error
	getServerDetails        func(context.Context, *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error)
}

func (m *mockSvc) GetAccount(ctx context.Context) (*upcloud.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getAccount(ctx)
}
func (m *mockSvc) GetStorageDetails(ctx context.Context, r *request.GetStorageDetailsRequest) (*upcloud.StorageDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getStorageDetails(ctx, r)
}
func (m *mockSvc) GetServersWithFilters(ctx context.Context, r *request.GetServersWithFiltersRequest) (*upcloud.Servers, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getServersWithFilters(ctx, r)
}
func (m *mockSvc) CreateServer(ctx context.Context, r *request.CreateServerRequest) (*upcloud.ServerDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createServer(ctx, r)
}
func (m *mockSvc) StopServer(ctx context.Context, r *request.StopServerRequest) (*upcloud.ServerDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopServer(ctx, r)
}
func (m *mockSvc) WaitForServerState(ctx context.Context, r *request.WaitForServerStateRequest) (*upcloud.ServerDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.waitForServerState(ctx, r)
}
func (m *mockSvc) DeleteServerAndStorages(ctx context.Context, r *request.DeleteServerAndStoragesRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteServerAndStorages(ctx, r)
}
func (m *mockSvc) GetServerDetails(ctx context.Context, r *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getServerDetails(ctx, r)
}

// newMockSvc returns a mock where every method panics unless overridden.
func newMockSvc() *mockSvc {
	panic := func(name string) { panic("unexpected call to mockSvc." + name) }
	return &mockSvc{
		getAccount: func(context.Context) (*upcloud.Account, error) { panic("GetAccount"); return nil, nil },
		getStorageDetails: func(context.Context, *request.GetStorageDetailsRequest) (*upcloud.StorageDetails, error) {
			panic("GetStorageDetails")
			return nil, nil
		},
		getServersWithFilters: func(context.Context, *request.GetServersWithFiltersRequest) (*upcloud.Servers, error) {
			panic("GetServersWithFilters")
			return nil, nil
		},
		createServer: func(context.Context, *request.CreateServerRequest) (*upcloud.ServerDetails, error) {
			panic("CreateServer")
			return nil, nil
		},
		stopServer: func(context.Context, *request.StopServerRequest) (*upcloud.ServerDetails, error) {
			panic("StopServer")
			return nil, nil
		},
		waitForServerState: func(context.Context, *request.WaitForServerStateRequest) (*upcloud.ServerDetails, error) {
			panic("WaitForServerState")
			return nil, nil
		},
		deleteServerAndStorages: func(context.Context, *request.DeleteServerAndStoragesRequest) error {
			panic("DeleteServerAndStorages")
			return nil
		},
		getServerDetails: func(context.Context, *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
			panic("GetServerDetails")
			return nil, nil
		},
	}
}

// baseGroup returns a minimal valid, initialized InstanceGroup with a pre-set mock service.
func baseGroup(svc *mockSvc) *InstanceGroup {
	g := &InstanceGroup{
		Token:    "test-token",
		Zone:     "fi-hel1",
		Template: "template-uuid",
		Name:     "test-group",
		Plan:     defaultPlan,
		svc:      svc,
		log:      hclog.NewNullLogger(),
	}
	g.initState()
	return g
}

// notFound is the error the UpCloud SDK returns for a missing resource.
var notFound = &upcloud.Problem{Status: http.StatusNotFound, Title: "SERVER_NOT_FOUND"}

// detailsInState returns a GetServerDetails mock reporting the given state.
func detailsInState(state string) func(context.Context, *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
	return func(context.Context, *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
		return &upcloud.ServerDetails{Server: upcloud.Server{State: state}}, nil
	}
}

// listServers returns a GetServersWithFilters mock listing the given servers.
func listServers(servers ...upcloud.Server) func(context.Context, *request.GetServersWithFiltersRequest) (*upcloud.Servers, error) {
	return func(context.Context, *request.GetServersWithFiltersRequest) (*upcloud.Servers, error) {
		return &upcloud.Servers{Servers: servers}, nil
	}
}

// ─── validate ────────────────────────────────────────────────────────────────

func TestValidate(t *testing.T) {
	tests := []struct {
		name            string
		g               InstanceGroup
		wantErr         bool
		wantPlan        string
		wantPrefix      string
		wantTitlePrefix string
		wantMaxSize     int
	}{
		{
			name:            "token auth - all required fields",
			g:               InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n"},
			wantPlan:        defaultPlan,
			wantPrefix:      defaultNamePrefix,
			wantTitlePrefix: defaultTitlePrefix,
			wantMaxSize:     defaultMaxSize,
		},
		{
			name: "username+password auth",
			g:    InstanceGroup{Username: "u", Password: "p", Zone: "z", Template: "t", Name: "n"},
		},
		{
			name:    "no auth at all",
			g:       InstanceGroup{Zone: "z", Template: "t", Name: "n"},
			wantErr: true,
		},
		{
			name:    "username without password",
			g:       InstanceGroup{Username: "u", Zone: "z", Template: "t", Name: "n"},
			wantErr: true,
		},
		{
			name:    "password without username",
			g:       InstanceGroup{Password: "p", Zone: "z", Template: "t", Name: "n"},
			wantErr: true,
		},
		{
			name:    "missing zone",
			g:       InstanceGroup{Token: "tok", Template: "t", Name: "n"},
			wantErr: true,
		},
		{
			name:    "missing template",
			g:       InstanceGroup{Token: "tok", Zone: "z", Name: "n"},
			wantErr: true,
		},
		{
			name:    "missing name",
			g:       InstanceGroup{Token: "tok", Zone: "z", Template: "t"},
			wantErr: true,
		},
		{
			name: "valid storage tier",
			g:    InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n", StorageTier: "maxiops"},
		},
		{
			name:    "invalid storage tier",
			g:       InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n", StorageTier: "MaxIOPS"},
			wantErr: true,
		},
		{
			name:    "negative storage size",
			g:       InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n", StorageSize: -1},
			wantErr: true,
		},
		{
			name:    "negative max size",
			g:       InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n", MaxSize: -1},
			wantErr: true,
		},
		{
			name:        "explicit plan preserved",
			g:           InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n", Plan: "2xCPU-4GB"},
			wantPlan:    "2xCPU-4GB",
			wantMaxSize: defaultMaxSize,
		},
		{
			name:        "explicit name prefix preserved",
			g:           InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n", NamePrefix: "ci"},
			wantPlan:    defaultPlan,
			wantPrefix:  "ci",
			wantMaxSize: defaultMaxSize,
		},
		{
			name:            "explicit title prefix preserved",
			g:               InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n", TitlePrefix: "ci-runner"},
			wantPlan:        defaultPlan,
			wantTitlePrefix: "ci-runner",
			wantMaxSize:     defaultMaxSize,
		},
		{
			name:            "overlong title prefix truncated to fit 255-char title",
			g:               InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n", TitlePrefix: strings.Repeat("t", 200)},
			wantPlan:        defaultPlan,
			wantTitlePrefix: strings.Repeat("t", maxTitlePrefixLen),
			wantMaxSize:     defaultMaxSize,
		},
		{
			name:        "explicit max size preserved",
			g:           InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n", MaxSize: 5},
			wantPlan:    defaultPlan,
			wantMaxSize: 5,
		},
	}

	// Index instead of copying: InstanceGroup holds locks.
	for i := range tests {
		tc := &tests[i]
		t.Run(tc.name, func(t *testing.T) {
			err := tc.g.validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("validate() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if tc.wantPlan != "" && tc.g.Plan != tc.wantPlan {
				t.Errorf("Plan = %q, want %q", tc.g.Plan, tc.wantPlan)
			}
			if tc.wantPrefix != "" && tc.g.NamePrefix != tc.wantPrefix {
				t.Errorf("NamePrefix = %q, want %q", tc.g.NamePrefix, tc.wantPrefix)
			}
			if tc.wantTitlePrefix != "" && tc.g.TitlePrefix != tc.wantTitlePrefix {
				t.Errorf("TitlePrefix = %q, want %q", tc.g.TitlePrefix, tc.wantTitlePrefix)
			}
			if tc.wantMaxSize != 0 && tc.g.MaxSize != tc.wantMaxSize {
				t.Errorf("MaxSize = %d, want %d", tc.g.MaxSize, tc.wantMaxSize)
			}
		})
	}
}

// ─── mapServerState ───────────────────────────────────────────────────────────

func TestMapServerState(t *testing.T) {
	tests := []struct {
		state string
		want  provider.State
	}{
		{upcloud.ServerStateStarted, provider.StateRunning},
		// Stopped/errored servers still exist and bill for storage: they stay
		// tracked (StateDeleting) while Update deletes them, instead of being
		// dropped (StateDeleted) and leaked.
		{upcloud.ServerStateStopped, provider.StateDeleting},
		{upcloud.ServerStateError, provider.StateDeleting},
		{"maintenance", provider.StateCreating},
		{"new", provider.StateCreating},
		{"", provider.StateCreating},
	}

	for _, tc := range tests {
		t.Run(tc.state, func(t *testing.T) {
			got := mapServerState(tc.state)
			if got != tc.want {
				t.Errorf("mapServerState(%q) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

// ─── randomSuffix ─────────────────────────────────────────────────────────────

func TestRandomSuffix(t *testing.T) {
	const allowed = "abcdefghijklmnopqrstuvwxyz0123456789"

	for _, n := range []int{0, 1, 8, 16} {
		s := randomSuffix(n)
		if len(s) != n {
			t.Errorf("randomSuffix(%d): len = %d, want %d", n, len(s), n)
		}
		for _, c := range s {
			if !strings.ContainsRune(allowed, c) {
				t.Errorf("randomSuffix(%d): unexpected character %q in %q", n, c, s)
			}
		}
	}

	// Two calls should almost always differ (birthday problem: 36^8 possibilities).
	if a, b := randomSuffix(8), randomSuffix(8); a == b {
		t.Logf("randomSuffix returned identical values twice (%q) — unlikely but not impossible", a)
	}
}

// ─── sanitizeNamePrefix ─────────────────────────────────────────────────────────

func TestSanitizeNamePrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"fleeting", "fleeting"},
		{"CI", "ci"},
		{"GitLab", "gitlab"},
		{"My_Runner", "my-runner"},
		{"My_Runner!", "my-runner"},
		{"--ci--", "ci"},
		{"a..b", "a-b"},
		{"  spaced  ", "spaced"},
		{"___", ""},
		{"", ""},
		// 60 chars → truncated to 54 so "<prefix>-<8 char suffix>" fits a 63-char label
		{strings.Repeat("a", 60), strings.Repeat("a", 54)},
		// truncation must not leave a trailing hyphen
		{strings.Repeat("a", 53) + "--bb", strings.Repeat("a", 53)},
	}
	for _, c := range cases {
		if got := sanitizeNamePrefix(c.in); got != c.want {
			t.Errorf("sanitizeNamePrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ─── Update ───────────────────────────────────────────────────────────────────

func TestUpdate(t *testing.T) {
	var filters []request.QueryFilter
	mock := newMockSvc()
	mock.getServersWithFilters = func(_ context.Context, r *request.GetServersWithFiltersRequest) (*upcloud.Servers, error) {
		filters = r.Filters
		return &upcloud.Servers{
			Servers: []upcloud.Server{
				{UUID: "uuid-1", State: upcloud.ServerStateStarted},
				{UUID: "uuid-2", State: upcloud.ServerStateStopped},
				{UUID: "uuid-3", State: "maintenance"},
			},
		}, nil
	}
	mock.deleteServerAndStorages = func(context.Context, *request.DeleteServerAndStoragesRequest) error { return nil }

	g := baseGroup(mock)
	seen := map[string]provider.State{}
	err := g.Update(context.Background(), func(id string, state provider.State) {
		seen[id] = state
	})
	g.cleanups.Wait()

	if err != nil {
		t.Fatalf("Update() unexpected error: %v", err)
	}
	want := map[string]provider.State{
		"uuid-1": provider.StateRunning,
		"uuid-2": provider.StateDeleting,
		"uuid-3": provider.StateCreating,
	}
	if len(seen) != len(want) {
		t.Fatalf("Update() reported %v, want %v", seen, want)
	}
	for id, state := range want {
		if seen[id] != state {
			t.Errorf("%s state = %v, want %v", id, seen[id], state)
		}
	}

	// The group label filter is what scopes the plugin to its own servers;
	// a wrong key or value would leak (or steal) servers.
	wantFilter := request.FilterLabel{Label: upcloud.Label{Key: groupLabelKey, Value: "test-group"}}
	if len(filters) != 1 || filters[0] != wantFilter {
		t.Errorf("GetServersWithFilters filters = %#v, want [%#v]", filters, wantFilter)
	}
}

func TestUpdate_APIError(t *testing.T) {
	mock := newMockSvc()
	mock.getServersWithFilters = func(_ context.Context, _ *request.GetServersWithFiltersRequest) (*upcloud.Servers, error) {
		return nil, errors.New("api error")
	}

	g := baseGroup(mock)
	if err := g.Update(context.Background(), func(string, provider.State) {}); err == nil {
		t.Fatal("Update() expected error, got nil")
	}
}

func TestUpdate_DeletesStoppedAndErroredServers(t *testing.T) {
	var deleted []string
	mock := newMockSvc()
	mock.getServersWithFilters = listServers(
		upcloud.Server{UUID: "uuid-running", State: upcloud.ServerStateStarted},
		upcloud.Server{UUID: "uuid-stopped", State: upcloud.ServerStateStopped},
		upcloud.Server{UUID: "uuid-error", State: upcloud.ServerStateError},
	)
	mock.deleteServerAndStorages = func(_ context.Context, r *request.DeleteServerAndStoragesRequest) error {
		deleted = append(deleted, r.UUID)
		return nil
	}

	g := baseGroup(mock)
	if err := g.Update(context.Background(), func(string, provider.State) {}); err != nil {
		t.Fatalf("Update() unexpected error: %v", err)
	}
	g.cleanups.Wait()

	slices.Sort(deleted)
	if want := []string{"uuid-error", "uuid-stopped"}; !slices.Equal(deleted, want) {
		t.Errorf("deleted = %v, want %v", deleted, want)
	}
}

func TestUpdate_ThrottlesDeleteRetries(t *testing.T) {
	attempts := 0
	mock := newMockSvc()
	mock.getServersWithFilters = listServers(upcloud.Server{UUID: "uuid-stopped", State: upcloud.ServerStateStopped})
	mock.deleteServerAndStorages = func(context.Context, *request.DeleteServerAndStoragesRequest) error {
		attempts++
		return errors.New("delete failed")
	}

	g := baseGroup(mock)
	update := func() {
		t.Helper()
		if err := g.Update(context.Background(), func(string, provider.State) {}); err != nil {
			t.Fatalf("Update() unexpected error: %v", err)
		}
		g.cleanups.Wait()
	}

	update()
	update() // within deleteRetryInterval: no new attempt
	if attempts != 1 {
		t.Fatalf("delete attempts = %d, want 1 (retries throttled)", attempts)
	}

	// Once the interval has passed, the failed delete is retried.
	g.mu.Lock()
	g.deleteAttempts["uuid-stopped"] = time.Now().Add(-deleteRetryInterval)
	g.mu.Unlock()
	update()
	if attempts != 2 {
		t.Errorf("delete attempts = %d, want 2 (retried after interval)", attempts)
	}
}

func TestUpdate_ForgetsServersThatAreGone(t *testing.T) {
	mock := newMockSvc()
	mock.getServersWithFilters = listServers(upcloud.Server{UUID: "uuid-1", State: upcloud.ServerStateStarted})

	g := baseGroup(mock)
	g.deleteAttempts["uuid-gone"] = time.Now()
	g.stopping["uuid-gone"] = struct{}{}

	if err := g.Update(context.Background(), func(string, provider.State) {}); err != nil {
		t.Fatalf("Update() unexpected error: %v", err)
	}
	if len(g.deleteAttempts) != 0 || len(g.stopping) != 0 {
		t.Errorf("bookkeeping not pruned: deleteAttempts=%v stopping=%v", g.deleteAttempts, g.stopping)
	}
}

// ─── Increase ─────────────────────────────────────────────────────────────────

func TestIncrease_AllSucceed(t *testing.T) {
	var created []string
	mock := newMockSvc()
	mock.createServer = func(_ context.Context, r *request.CreateServerRequest) (*upcloud.ServerDetails, error) {
		created = append(created, r.Hostname)
		return &upcloud.ServerDetails{}, nil
	}

	g := baseGroup(mock)
	n, err := g.Increase(context.Background(), 3)

	if err != nil {
		t.Fatalf("Increase() unexpected error: %v", err)
	}
	if n != 3 {
		t.Errorf("Increase() = %d, want 3", n)
	}
	if len(created) != 3 {
		t.Errorf("CreateServer called %d times, want 3", len(created))
	}
}

func TestIncrease_CreateRequest(t *testing.T) {
	var req *request.CreateServerRequest
	mock := newMockSvc()
	mock.createServer = func(_ context.Context, r *request.CreateServerRequest) (*upcloud.ServerDetails, error) {
		req = r
		return &upcloud.ServerDetails{}, nil
	}

	g := baseGroup(mock)
	g.NamePrefix = "ci"
	g.TitlePrefix = "CI Runner"
	g.StorageSize = 20
	g.StorageTier = upcloud.StorageTierMaxIOPS
	g.publicKey = "ssh-ed25519 AAAA test\n"
	g.settings.Username = "root"

	if _, err := g.Increase(context.Background(), 1); err != nil {
		t.Fatalf("Increase() unexpected error: %v", err)
	}

	if !strings.HasPrefix(req.Hostname, "ci-") || len(req.Hostname) != len("ci-")+8 {
		t.Errorf("Hostname = %q, want ci-<8 chars>", req.Hostname)
	}
	if want := "CI Runner - " + req.Hostname; req.Title != want {
		t.Errorf("Title = %q, want %q", req.Title, want)
	}
	if req.Zone != "fi-hel1" || req.Plan != defaultPlan {
		t.Errorf("Zone/Plan = %q/%q, want fi-hel1/%s", req.Zone, req.Plan, defaultPlan)
	}
	if req.Metadata != upcloud.True {
		t.Error("Metadata service not enabled; cloud-init can't inject the SSH key")
	}
	// Update finds the group's servers by this label.
	if req.Labels == nil || len(*req.Labels) != 1 || (*req.Labels)[0] != (upcloud.Label{Key: groupLabelKey, Value: "test-group"}) {
		t.Errorf("Labels = %v, want [%s=test-group]", req.Labels, groupLabelKey)
	}

	if len(req.StorageDevices) != 1 {
		t.Fatalf("StorageDevices = %v, want one clone of the template", req.StorageDevices)
	}
	disk := req.StorageDevices[0]
	if disk.Action != request.CreateServerStorageDeviceActionClone || disk.Storage != "template-uuid" || disk.Size != 20 || disk.Tier != upcloud.StorageTierMaxIOPS {
		t.Errorf("storage device = %+v, want clone of template-uuid, 20 GB, maxiops", disk)
	}

	if req.LoginUser == nil || req.LoginUser.Username != "root" || !slices.Equal([]string(req.LoginUser.SSHKeys), []string{g.publicKey}) {
		t.Errorf("LoginUser = %+v, want root with the connector key", req.LoginUser)
	}

	ifaces := req.Networking.Interfaces
	if len(ifaces) != 1 || ifaces[0].Type != upcloud.NetworkTypePublic {
		t.Errorf("interfaces = %+v, want a single public interface", ifaces)
	}
}

func TestIncrease_NoSSHKey(t *testing.T) {
	var req *request.CreateServerRequest
	mock := newMockSvc()
	mock.createServer = func(_ context.Context, r *request.CreateServerRequest) (*upcloud.ServerDetails, error) {
		req = r
		return &upcloud.ServerDetails{}, nil
	}

	g := baseGroup(mock)
	if _, err := g.Increase(context.Background(), 1); err != nil {
		t.Fatalf("Increase() unexpected error: %v", err)
	}
	if req.LoginUser != nil {
		t.Errorf("LoginUser = %+v, want nil without a connector key", req.LoginUser)
	}
}

func TestIncrease_PartialFailure(t *testing.T) {
	calls := 0
	mock := newMockSvc()
	mock.createServer = func(_ context.Context, _ *request.CreateServerRequest) (*upcloud.ServerDetails, error) {
		calls++
		if calls%2 == 0 {
			return nil, errors.New("quota exceeded")
		}
		return &upcloud.ServerDetails{}, nil
	}

	g := baseGroup(mock)
	n, err := g.Increase(context.Background(), 4)

	// Failures are aggregated into the returned error so the taskscaler can back off.
	if err == nil {
		t.Fatal("Increase() expected error for partial failure, got nil")
	}
	if n != 2 {
		t.Errorf("Increase() = %d, want 2 (half succeed)", n)
	}
}

func TestIncrease_AllFail(t *testing.T) {
	mock := newMockSvc()
	mock.createServer = func(_ context.Context, _ *request.CreateServerRequest) (*upcloud.ServerDetails, error) {
		return nil, errors.New("HOSTNAME_INVALID")
	}

	g := baseGroup(mock)
	n, err := g.Increase(context.Background(), 2)

	if err == nil {
		t.Fatal("Increase() expected error when all creates fail, got nil")
	}
	if !strings.Contains(err.Error(), "HOSTNAME_INVALID") {
		t.Errorf("Increase() error = %v, want it to wrap the API error", err)
	}
	if n != 0 {
		t.Errorf("Increase() = %d, want 0", n)
	}
}

func TestIncrease_CancelledContext(t *testing.T) {
	// createServer intentionally left as a panic: no creates may start once
	// the caller's context is cancelled.
	mock := newMockSvc()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	g := baseGroup(mock)
	n, err := g.Increase(ctx, 3)

	if err == nil {
		t.Fatal("Increase() expected error for cancelled context, got nil")
	}
	if n != 0 {
		t.Errorf("Increase() = %d, want 0", n)
	}
}

func TestIncrease_Zero(t *testing.T) {
	g := baseGroup(newMockSvc())
	n, err := g.Increase(context.Background(), 0)
	if err != nil || n != 0 {
		t.Errorf("Increase(0) = (%d, %v), want (0, nil)", n, err)
	}
}

func TestIncrease_SetsUserData(t *testing.T) {
	var got string
	mock := newMockSvc()
	mock.createServer = func(_ context.Context, r *request.CreateServerRequest) (*upcloud.ServerDetails, error) {
		got = r.UserData
		return &upcloud.ServerDetails{}, nil
	}

	g := baseGroup(mock)
	g.UserData = "https://example.com/init.sh"
	if _, err := g.Increase(context.Background(), 1); err != nil {
		t.Fatalf("Increase() unexpected error: %v", err)
	}

	if got != g.UserData {
		t.Errorf("CreateServer UserData = %q, want %q", got, g.UserData)
	}
}

// ─── Decrease ─────────────────────────────────────────────────────────────────

func TestDecrease_RunningServersAreStoppedNotWaitedOn(t *testing.T) {
	var stopped []string
	mock := newMockSvc()
	mock.getServerDetails = detailsInState(upcloud.ServerStateStarted)
	mock.stopServer = func(_ context.Context, r *request.StopServerRequest) (*upcloud.ServerDetails, error) {
		if r.StopType != request.ServerStopTypeHard {
			t.Errorf("StopType = %q, want hard", r.StopType)
		}
		stopped = append(stopped, r.UUID)
		return &upcloud.ServerDetails{}, nil
	}
	// waitForServerState and deleteServerAndStorages intentionally left as
	// panics: Decrease must not block on the stop; Update deletes the server
	// once it is stopped.

	g := baseGroup(mock)
	instances := []string{"uuid-1", "uuid-2", "uuid-3"}
	succeeded, err := g.Decrease(context.Background(), instances)

	if err != nil {
		t.Fatalf("Decrease() unexpected error: %v", err)
	}
	slices.Sort(succeeded)
	slices.Sort(stopped)
	if !slices.Equal(succeeded, instances) || !slices.Equal(stopped, instances) {
		t.Errorf("succeeded = %v, stopped = %v, want both %v", succeeded, stopped, instances)
	}
	if len(g.stopping) != 3 {
		t.Errorf("stopping = %v, want all 3 tracked for Shutdown", g.stopping)
	}
}

func TestDecrease_PartialFailure(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = detailsInState(upcloud.ServerStateStarted)
	mock.stopServer = func(_ context.Context, r *request.StopServerRequest) (*upcloud.ServerDetails, error) {
		if r.UUID == "uuid-bad" {
			return nil, errors.New("stop failed")
		}
		return &upcloud.ServerDetails{}, nil
	}

	g := baseGroup(mock)
	succeeded, err := g.Decrease(context.Background(), []string{"uuid-ok", "uuid-bad"})

	if err == nil {
		t.Fatal("Decrease() expected error for partial failure, got nil")
	}
	if len(succeeded) != 1 || succeeded[0] != "uuid-ok" {
		t.Errorf("Decrease() succeeded = %v, want [uuid-ok]", succeeded)
	}
}

func TestDecrease_StoppedOrErroredDeletedDirectly(t *testing.T) {
	for _, state := range []string{upcloud.ServerStateStopped, upcloud.ServerStateError} {
		t.Run(state, func(t *testing.T) {
			deleted := false
			mock := newMockSvc()
			mock.getServerDetails = detailsInState(state)
			// stopServer intentionally left as a panic: a stop attempt would fail.
			mock.deleteServerAndStorages = func(context.Context, *request.DeleteServerAndStoragesRequest) error {
				deleted = true
				return nil
			}

			g := baseGroup(mock)
			succeeded, err := g.Decrease(context.Background(), []string{"uuid-1"})

			if err != nil {
				t.Fatalf("Decrease() unexpected error: %v", err)
			}
			if !deleted {
				t.Error("Decrease() did not delete the server")
			}
			if len(succeeded) != 1 {
				t.Errorf("Decrease() succeeded = %v, want [uuid-1]", succeeded)
			}
		})
	}
}

func TestDecrease_DeleteFailure(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = detailsInState(upcloud.ServerStateStopped)
	mock.deleteServerAndStorages = func(context.Context, *request.DeleteServerAndStoragesRequest) error {
		return errors.New("delete failed")
	}

	g := baseGroup(mock)
	succeeded, err := g.Decrease(context.Background(), []string{"uuid-1"})
	if err == nil || len(succeeded) != 0 {
		t.Errorf("Decrease() = (%v, %v), want ([], error)", succeeded, err)
	}
}

func TestDecrease_AlreadyGoneCountsAsRemoved(t *testing.T) {
	t.Run("details 404", func(t *testing.T) {
		mock := newMockSvc()
		mock.getServerDetails = func(context.Context, *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
			return nil, notFound
		}

		g := baseGroup(mock)
		succeeded, err := g.Decrease(context.Background(), []string{"uuid-1"})
		if err != nil || len(succeeded) != 1 {
			t.Errorf("Decrease() = (%v, %v), want ([uuid-1], nil)", succeeded, err)
		}
	})

	t.Run("delete 404", func(t *testing.T) {
		mock := newMockSvc()
		mock.getServerDetails = detailsInState(upcloud.ServerStateStopped)
		mock.deleteServerAndStorages = func(context.Context, *request.DeleteServerAndStoragesRequest) error {
			return notFound
		}

		g := baseGroup(mock)
		succeeded, err := g.Decrease(context.Background(), []string{"uuid-1"})
		if err != nil || len(succeeded) != 1 {
			t.Errorf("Decrease() = (%v, %v), want ([uuid-1], nil)", succeeded, err)
		}
	})
}

func TestDecrease_DetailsError(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = func(context.Context, *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
		return nil, errors.New("api error")
	}

	g := baseGroup(mock)
	succeeded, err := g.Decrease(context.Background(), []string{"uuid-1"})
	if err == nil || len(succeeded) != 0 {
		t.Errorf("Decrease() = (%v, %v), want ([], error)", succeeded, err)
	}
}

func TestDecrease_Empty(t *testing.T) {
	g := baseGroup(newMockSvc())
	succeeded, err := g.Decrease(context.Background(), nil)
	if err != nil || len(succeeded) != 0 {
		t.Errorf("Decrease(nil) = (%v, %v), want ([], nil)", succeeded, err)
	}
}

// ─── Shutdown ─────────────────────────────────────────────────────────────────

func TestShutdown_FinishesStoppedServers(t *testing.T) {
	var deleted []string
	var hadDeadline bool
	mock := newMockSvc()
	mock.getServerDetails = func(_ context.Context, r *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
		switch r.UUID {
		case "uuid-stopping":
			return &upcloud.ServerDetails{Server: upcloud.Server{State: "maintenance"}}, nil
		case "uuid-stopped":
			return &upcloud.ServerDetails{Server: upcloud.Server{State: upcloud.ServerStateStopped}}, nil
		default:
			return nil, notFound
		}
	}
	mock.waitForServerState = func(ctx context.Context, r *request.WaitForServerStateRequest) (*upcloud.ServerDetails, error) {
		if r.UUID != "uuid-stopping" {
			t.Errorf("WaitForServerState called for %s, want only uuid-stopping", r.UUID)
		}
		_, hadDeadline = ctx.Deadline()
		return &upcloud.ServerDetails{}, nil
	}
	mock.deleteServerAndStorages = func(_ context.Context, r *request.DeleteServerAndStoragesRequest) error {
		deleted = append(deleted, r.UUID)
		return nil
	}

	g := baseGroup(mock)
	for _, uuid := range []string{"uuid-stopping", "uuid-stopped", "uuid-gone"} {
		g.stopping[uuid] = struct{}{}
	}

	if err := g.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() unexpected error: %v", err)
	}
	slices.Sort(deleted)
	if want := []string{"uuid-stopped", "uuid-stopping"}; !slices.Equal(deleted, want) {
		t.Errorf("deleted = %v, want %v", deleted, want)
	}
	if !hadDeadline {
		t.Error("WaitForServerState ctx has no deadline; an unresponsive server would hang Shutdown forever")
	}
	if g.ctx.Err() == nil {
		t.Error("Shutdown() did not cancel the background context")
	}
}

func TestShutdown_ReportsFailures(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = detailsInState(upcloud.ServerStateStopped)
	mock.deleteServerAndStorages = func(context.Context, *request.DeleteServerAndStoragesRequest) error {
		return errors.New("delete failed")
	}

	g := baseGroup(mock)
	g.stopping["uuid-1"] = struct{}{}
	if err := g.Shutdown(context.Background()); err == nil {
		t.Error("Shutdown() expected error when a delete fails, got nil")
	}
}

func TestShutdown_StopWaitFailure(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = detailsInState("maintenance")
	mock.waitForServerState = func(context.Context, *request.WaitForServerStateRequest) (*upcloud.ServerDetails, error) {
		return nil, context.DeadlineExceeded
	}
	// deleteServerAndStorages intentionally left as a panic: a server that
	// never stopped must not be deleted.

	g := baseGroup(mock)
	g.stopping["uuid-1"] = struct{}{}
	if err := g.Shutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown() error = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

func TestShutdown_WaitsForBackgroundDeletions(t *testing.T) {
	release := make(chan struct{})
	deleted := false
	mock := newMockSvc()
	mock.getServersWithFilters = listServers(upcloud.Server{UUID: "uuid-stopped", State: upcloud.ServerStateStopped})
	mock.deleteServerAndStorages = func(context.Context, *request.DeleteServerAndStoragesRequest) error {
		<-release
		deleted = true
		return nil
	}

	g := baseGroup(mock)
	if err := g.Update(context.Background(), func(string, provider.State) {}); err != nil {
		t.Fatalf("Update() unexpected error: %v", err)
	}

	done := make(chan error)
	go func() { done <- g.Shutdown(context.Background()) }()

	select {
	case err := <-done:
		t.Fatalf("Shutdown() returned (%v) while a background deletion was still running", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Shutdown() unexpected error: %v", err)
	}
	if !deleted {
		t.Error("background deletion did not complete")
	}
}

func TestShutdown_ContextCancelled(t *testing.T) {
	g := baseGroup(newMockSvc())
	g.cleanups.Add(1) // a background deletion that never finishes
	defer g.cleanups.Done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Shutdown() error = %v, want context.Canceled", err)
	}
}

func TestShutdown_BeforeInit(t *testing.T) {
	g := &InstanceGroup{}
	if err := g.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown() on an uninitialized group = %v, want nil", err)
	}
}

// ─── ConnectInfo ──────────────────────────────────────────────────────────────

func makeDetails(publicIP, privateIP string) *upcloud.ServerDetails {
	d := &upcloud.ServerDetails{}
	if publicIP != "" {
		d.IPAddresses = append(d.IPAddresses, upcloud.IPAddress{
			Family:  upcloud.IPAddressFamilyIPv4,
			Access:  upcloud.IPAddressAccessPublic,
			Address: publicIP,
		})
	}
	if privateIP != "" {
		d.IPAddresses = append(d.IPAddresses, upcloud.IPAddress{
			Family:  upcloud.IPAddressFamilyIPv4,
			Access:  upcloud.IPAddressAccessPrivate,
			Address: privateIP,
		})
	}
	return d
}

func TestConnectInfo_Defaults(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = func(_ context.Context, _ *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
		return makeDetails("1.2.3.4", ""), nil
	}

	g := baseGroup(mock)
	info, err := g.ConnectInfo(context.Background(), "uuid-1")

	if err != nil {
		t.Fatalf("ConnectInfo() unexpected error: %v", err)
	}
	if info.OS != "linux" {
		t.Errorf("OS = %q, want linux", info.OS)
	}
	if info.Arch != "amd64" {
		t.Errorf("Arch = %q, want amd64", info.Arch)
	}
	if info.Protocol != provider.ProtocolSSH {
		t.Errorf("Protocol = %v, want SSH", info.Protocol)
	}
	if info.ExternalAddr != "1.2.3.4" {
		t.Errorf("ExternalAddr = %q, want 1.2.3.4", info.ExternalAddr)
	}
}

func TestConnectInfo_PreservesConnectorConfig(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = func(_ context.Context, _ *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
		return makeDetails("1.2.3.4", ""), nil
	}

	g := baseGroup(mock)
	g.settings = provider.Settings{
		ConnectorConfig: provider.ConnectorConfig{OS: "linux", Arch: "arm64", Username: "runner"},
	}
	info, err := g.ConnectInfo(context.Background(), "uuid-1")

	if err != nil {
		t.Fatalf("ConnectInfo() unexpected error: %v", err)
	}
	if info.Arch != "arm64" {
		t.Errorf("Arch = %q, want arm64 (from ConnectorConfig)", info.Arch)
	}
	if info.Username != "runner" {
		t.Errorf("Username = %q, want runner", info.Username)
	}
}

func TestConnectInfo_UsePrivateNetwork(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = func(_ context.Context, _ *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
		return makeDetails("1.2.3.4", "10.0.0.5"), nil
	}

	g := baseGroup(mock)
	g.UsePrivateNetwork = true
	info, err := g.ConnectInfo(context.Background(), "uuid-1")

	if err != nil {
		t.Fatalf("ConnectInfo() unexpected error: %v", err)
	}
	if info.ExternalAddr != "10.0.0.5" {
		t.Errorf("ExternalAddr = %q, want private IP 10.0.0.5", info.ExternalAddr)
	}
	if info.InternalAddr != "10.0.0.5" {
		t.Errorf("InternalAddr = %q, want 10.0.0.5", info.InternalAddr)
	}
}

func TestConnectInfo_UsePrivateNetworkFallsBackToPublic(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = func(_ context.Context, _ *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
		return makeDetails("1.2.3.4", ""), nil
	}

	g := baseGroup(mock)
	g.UsePrivateNetwork = true
	info, err := g.ConnectInfo(context.Background(), "uuid-1")

	if err != nil {
		t.Fatalf("ConnectInfo() unexpected error: %v", err)
	}
	if info.ExternalAddr != "1.2.3.4" {
		t.Errorf("ExternalAddr = %q, want public IP 1.2.3.4", info.ExternalAddr)
	}
}

func TestConnectInfo_APIError(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = func(_ context.Context, _ *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
		return nil, errors.New("not found")
	}

	g := baseGroup(mock)
	if _, err := g.ConnectInfo(context.Background(), "uuid-1"); err == nil {
		t.Fatal("ConnectInfo() expected error, got nil")
	}
}

// ─── Heartbeat ────────────────────────────────────────────────────────────────

func TestHeartbeat_HealthyServer(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = detailsInState(upcloud.ServerStateStarted)

	g := baseGroup(mock)
	if err := g.Heartbeat(context.Background(), "uuid-1"); err != nil {
		t.Errorf("Heartbeat() unexpected error for healthy server: %v", err)
	}
}

func TestHeartbeat_UnhealthyStates(t *testing.T) {
	for _, state := range []string{upcloud.ServerStateError, upcloud.ServerStateStopped} {
		t.Run(state, func(t *testing.T) {
			mock := newMockSvc()
			mock.getServerDetails = detailsInState(state)

			g := baseGroup(mock)
			if err := g.Heartbeat(context.Background(), "uuid-1"); err == nil {
				t.Errorf("Heartbeat() expected error for server in %s state, got nil", state)
			}
		})
	}
}

func TestHeartbeat_APIErrorTreatedAsHealthy(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = func(_ context.Context, _ *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
		return nil, errors.New("transient network error")
	}

	g := baseGroup(mock)
	if err := g.Heartbeat(context.Background(), "uuid-1"); err != nil {
		t.Errorf("Heartbeat() should treat API errors as healthy, got: %v", err)
	}
}

func TestHeartbeat_NotFoundIsUnhealthy(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = func(_ context.Context, _ *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
		return nil, notFound
	}

	g := baseGroup(mock)
	if err := g.Heartbeat(context.Background(), "uuid-1"); err == nil {
		t.Error("Heartbeat() expected error for 404 (server gone), got nil")
	}
}

// ─── Suspend / Resume ─────────────────────────────────────────────────────────

func TestSuspendResume_NotSupported(t *testing.T) {
	g := baseGroup(newMockSvc())

	if _, err := g.Suspend(context.Background(), []string{"uuid-1"}); !errors.Is(err, provider.ErrCapabilityNotSupported) {
		t.Errorf("Suspend() error = %v, want ErrCapabilityNotSupported", err)
	}
	if _, err := g.Resume(context.Background(), []string{"uuid-1"}); !errors.Is(err, provider.ErrCapabilityNotSupported) {
		t.Errorf("Resume() error = %v, want ErrCapabilityNotSupported", err)
	}
}

// ─── Init ─────────────────────────────────────────────────────────────────────

// useMock makes Init use mock instead of the real UpCloud service.
func useMock(t *testing.T, mock *mockSvc) {
	t.Helper()
	orig := newUpcloudService
	newUpcloudService = func(_ *client.Client) upcloudSvc { return mock }
	t.Cleanup(func() { newUpcloudService = orig })
}

// initMock returns a mock that accepts the credentials and template checks.
func initMock() *mockSvc {
	mock := newMockSvc()
	mock.getAccount = func(context.Context) (*upcloud.Account, error) {
		return &upcloud.Account{}, nil
	}
	mock.getStorageDetails = func(context.Context, *request.GetStorageDetailsRequest) (*upcloud.StorageDetails, error) {
		return &upcloud.StorageDetails{}, nil
	}
	return mock
}

func TestInit_InvalidConfig(t *testing.T) {
	useMock(t, newMockSvc())

	g := &InstanceGroup{Token: "tok", Zone: "z", Name: "n"}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err == nil {
		t.Fatal("Init() expected error for missing template, got nil")
	}
}

func TestInit_InvalidSSHKey(t *testing.T) {
	useMock(t, newMockSvc())

	g := &InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n"}
	settings := provider.Settings{
		ConnectorConfig: provider.ConnectorConfig{Key: []byte("not-a-valid-pem-key")},
	}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), settings); err == nil {
		t.Fatal("Init() expected error for invalid SSH key, got nil")
	}
}

func TestInit_DerivesPublicKey(t *testing.T) {
	useMock(t, initMock())

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	g := &InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n"}
	settings := provider.Settings{ConnectorConfig: provider.ConnectorConfig{Key: pem.EncodeToMemory(block)}}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), settings); err != nil {
		t.Fatalf("Init() unexpected error: %v", err)
	}
	if want := string(ssh.MarshalAuthorizedKey(sshPub)); g.publicKey != want {
		t.Errorf("publicKey = %q, want %q", g.publicKey, want)
	}
}

func TestInit_GetAccountError(t *testing.T) {
	mock := newMockSvc()
	mock.getAccount = func(context.Context) (*upcloud.Account, error) {
		return nil, errors.New("invalid credentials")
	}
	useMock(t, mock)

	g := &InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n"}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err == nil {
		t.Fatal("Init() expected error when GetAccount fails, got nil")
	}
}

func TestInit_TemplateNotFound(t *testing.T) {
	mock := initMock()
	mock.getStorageDetails = func(context.Context, *request.GetStorageDetailsRequest) (*upcloud.StorageDetails, error) {
		return nil, &upcloud.Problem{Status: http.StatusNotFound, Title: "STORAGE_NOT_FOUND"}
	}
	useMock(t, mock)

	g := &InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n"}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err == nil {
		t.Fatal("Init() expected error for a missing template, got nil")
	}
}

func TestInit_TemplateCheckErrorOnlyWarns(t *testing.T) {
	mock := initMock()
	mock.getStorageDetails = func(context.Context, *request.GetStorageDetailsRequest) (*upcloud.StorageDetails, error) {
		return nil, &upcloud.Problem{Status: http.StatusForbidden, Title: "FORBIDDEN"}
	}
	useMock(t, mock)

	g := &InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n"}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err != nil {
		t.Fatalf("Init() unexpected error: %v", err)
	}
}

func TestInit_Success(t *testing.T) {
	useMock(t, initMock())

	g := &InstanceGroup{Token: "tok", Zone: "fi-hel1", Template: "t", Name: "n"}
	info, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})

	if err != nil {
		t.Fatalf("Init() unexpected error: %v", err)
	}
	if info.MaxSize != defaultMaxSize {
		t.Errorf("ProviderInfo.MaxSize = %d, want %d", info.MaxSize, defaultMaxSize)
	}
	if info.ID != "upcloud/fi-hel1/n" {
		t.Errorf("ProviderInfo.ID = %q, want upcloud/fi-hel1/n", info.ID)
	}
	if g.ctx == nil || g.stopping == nil || g.deleteAttempts == nil {
		t.Error("Init() did not initialize background deletion state")
	}
	if err := g.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown() unexpected error: %v", err)
	}
}
