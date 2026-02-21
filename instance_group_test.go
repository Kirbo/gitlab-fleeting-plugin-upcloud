package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	upcloud "github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/client"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// ─── mock ────────────────────────────────────────────────────────────────────

// mockSvc is a test double for upcloudSvc.
// Each field is a function so individual tests can customise behaviour.
type mockSvc struct {
	mu                      sync.Mutex
	getAccount              func(context.Context) (*upcloud.Account, error)
	getServersWithFilters   func(context.Context, *request.GetServersWithFiltersRequest) (*upcloud.Servers, error)
	createServer            func(context.Context, *request.CreateServerRequest) (*upcloud.ServerDetails, error)
	stopServer              func(context.Context, *request.StopServerRequest) (*upcloud.ServerDetails, error)
	waitForServerState      func(context.Context, *request.WaitForServerStateRequest) (*upcloud.ServerDetails, error)
	deleteServerAndStorages func(context.Context, *request.DeleteServerAndStoragesRequest) error
	getServerDetails        func(context.Context, *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error)
}

func (m *mockSvc) GetAccount(ctx context.Context) (*upcloud.Account, error) {
	return m.getAccount(ctx)
}
func (m *mockSvc) GetServersWithFilters(ctx context.Context, r *request.GetServersWithFiltersRequest) (*upcloud.Servers, error) {
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
	return m.getServerDetails(ctx, r)
}

// newMockSvc returns a mock where every method panics unless overridden.
func newMockSvc() *mockSvc {
	panic := func(name string) { panic("unexpected call to mockSvc." + name) }
	return &mockSvc{
		getAccount:              func(context.Context) (*upcloud.Account, error) { panic("GetAccount"); return nil, nil },
		getServersWithFilters:   func(context.Context, *request.GetServersWithFiltersRequest) (*upcloud.Servers, error) { panic("GetServersWithFilters"); return nil, nil },
		createServer:            func(context.Context, *request.CreateServerRequest) (*upcloud.ServerDetails, error) { panic("CreateServer"); return nil, nil },
		stopServer:              func(context.Context, *request.StopServerRequest) (*upcloud.ServerDetails, error) { panic("StopServer"); return nil, nil },
		waitForServerState:      func(context.Context, *request.WaitForServerStateRequest) (*upcloud.ServerDetails, error) { panic("WaitForServerState"); return nil, nil },
		deleteServerAndStorages: func(context.Context, *request.DeleteServerAndStoragesRequest) error { panic("DeleteServerAndStorages"); return nil },
		getServerDetails:        func(context.Context, *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) { panic("GetServerDetails"); return nil, nil },
	}
}

// baseGroup returns a minimal valid InstanceGroup with a pre-set mock service.
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
	return g
}

// ─── validate ────────────────────────────────────────────────────────────────

func TestValidate(t *testing.T) {
	tests := []struct {
		name        string
		g           InstanceGroup
		wantErr     bool
		wantPlan    string
		wantPrefix  string
		wantMaxSize int
	}{
		{
			name:        "token auth - all required fields",
			g:           InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n"},
			wantPlan:    defaultPlan,
			wantPrefix:  defaultNamePrefix,
			wantMaxSize: defaultMaxSize,
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
			name:        "explicit max size preserved",
			g:           InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n", MaxSize: 5},
			wantPlan:    defaultPlan,
			wantMaxSize: 5,
		},
	}

	for _, tc := range tests {
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
		{upcloud.ServerStateStopped, provider.StateDeleted},
		{upcloud.ServerStateError, provider.StateDeleted},
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

// ─── Update ───────────────────────────────────────────────────────────────────

func TestUpdate(t *testing.T) {
	mock := newMockSvc()
	mock.getServersWithFilters = func(_ context.Context, _ *request.GetServersWithFiltersRequest) (*upcloud.Servers, error) {
		return &upcloud.Servers{
			Servers: []upcloud.Server{
				{UUID: "uuid-1", State: upcloud.ServerStateStarted},
				{UUID: "uuid-2", State: upcloud.ServerStateStopped},
			},
		}, nil
	}

	g := baseGroup(mock)
	seen := map[string]provider.State{}
	err := g.Update(context.Background(), func(id string, state provider.State) {
		seen[id] = state
	})

	if err != nil {
		t.Fatalf("Update() unexpected error: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("Update() reported %d instances, want 2", len(seen))
	}
	if seen["uuid-1"] != provider.StateRunning {
		t.Errorf("uuid-1 state = %v, want StateRunning", seen["uuid-1"])
	}
	if seen["uuid-2"] != provider.StateDeleted {
		t.Errorf("uuid-2 state = %v, want StateDeleted", seen["uuid-2"])
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

	// Increase never returns an error; it logs failures and counts successes.
	if err != nil {
		t.Fatalf("Increase() unexpected error: %v", err)
	}
	if n != 2 {
		t.Errorf("Increase() = %d, want 2 (half succeed)", n)
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
	g.Increase(context.Background(), 1)

	if got != g.UserData {
		t.Errorf("CreateServer UserData = %q, want %q", got, g.UserData)
	}
}

// ─── Decrease ─────────────────────────────────────────────────────────────────

func TestDecrease_AllSucceed(t *testing.T) {
	mock := newMockSvc()
	mock.stopServer = func(_ context.Context, _ *request.StopServerRequest) (*upcloud.ServerDetails, error) {
		return &upcloud.ServerDetails{}, nil
	}
	mock.waitForServerState = func(_ context.Context, _ *request.WaitForServerStateRequest) (*upcloud.ServerDetails, error) {
		return &upcloud.ServerDetails{}, nil
	}
	mock.deleteServerAndStorages = func(_ context.Context, _ *request.DeleteServerAndStoragesRequest) error {
		return nil
	}

	g := baseGroup(mock)
	instances := []string{"uuid-1", "uuid-2", "uuid-3"}
	succeeded, err := g.Decrease(context.Background(), instances)

	if err != nil {
		t.Fatalf("Decrease() unexpected error: %v", err)
	}
	if len(succeeded) != 3 {
		t.Errorf("Decrease() succeeded = %d, want 3", len(succeeded))
	}
}

func TestDecrease_PartialFailure(t *testing.T) {
	mock := newMockSvc()
	mock.stopServer = func(_ context.Context, r *request.StopServerRequest) (*upcloud.ServerDetails, error) {
		if r.UUID == "uuid-bad" {
			return nil, errors.New("stop failed")
		}
		return &upcloud.ServerDetails{}, nil
	}
	mock.waitForServerState = func(_ context.Context, _ *request.WaitForServerStateRequest) (*upcloud.ServerDetails, error) {
		return &upcloud.ServerDetails{}, nil
	}
	mock.deleteServerAndStorages = func(_ context.Context, _ *request.DeleteServerAndStoragesRequest) error {
		return nil
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

func TestDecrease_Empty(t *testing.T) {
	g := baseGroup(newMockSvc())
	succeeded, err := g.Decrease(context.Background(), nil)
	if err != nil || len(succeeded) != 0 {
		t.Errorf("Decrease(nil) = (%v, %v), want ([], nil)", succeeded, err)
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
	mock.getServerDetails = func(_ context.Context, _ *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
		return &upcloud.ServerDetails{Server: upcloud.Server{State: upcloud.ServerStateStarted}}, nil
	}

	g := baseGroup(mock)
	if err := g.Heartbeat(context.Background(), "uuid-1"); err != nil {
		t.Errorf("Heartbeat() unexpected error for healthy server: %v", err)
	}
}

func TestHeartbeat_ErrorState(t *testing.T) {
	mock := newMockSvc()
	mock.getServerDetails = func(_ context.Context, _ *request.GetServerDetailsRequest) (*upcloud.ServerDetails, error) {
		return &upcloud.ServerDetails{Server: upcloud.Server{State: upcloud.ServerStateError}}, nil
	}

	g := baseGroup(mock)
	if err := g.Heartbeat(context.Background(), "uuid-1"); err == nil {
		t.Error("Heartbeat() expected error for server in error state, got nil")
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

// ─── Init ─────────────────────────────────────────────────────────────────────

func TestInit_InvalidSSHKey(t *testing.T) {
	orig := newUpcloudService
	newUpcloudService = func(_ *client.Client) upcloudSvc { return newMockSvc() }
	defer func() { newUpcloudService = orig }()

	g := &InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n"}
	settings := provider.Settings{
		ConnectorConfig: provider.ConnectorConfig{Key: []byte("not-a-valid-pem-key")},
	}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), settings); err == nil {
		t.Fatal("Init() expected error for invalid SSH key, got nil")
	}
}

func TestInit_GetAccountError(t *testing.T) {
	mock := newMockSvc()
	mock.getAccount = func(context.Context) (*upcloud.Account, error) {
		return nil, errors.New("invalid credentials")
	}

	orig := newUpcloudService
	newUpcloudService = func(_ *client.Client) upcloudSvc { return mock }
	defer func() { newUpcloudService = orig }()

	g := &InstanceGroup{Token: "tok", Zone: "z", Template: "t", Name: "n"}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err == nil {
		t.Fatal("Init() expected error when GetAccount fails, got nil")
	}
}

func TestInit_Success(t *testing.T) {
	mock := newMockSvc()
	mock.getAccount = func(context.Context) (*upcloud.Account, error) {
		return &upcloud.Account{}, nil
	}

	orig := newUpcloudService
	newUpcloudService = func(_ *client.Client) upcloudSvc { return mock }
	defer func() { newUpcloudService = orig }()

	g := &InstanceGroup{Token: "tok", Zone: "fi-hel1", Template: "t", Name: "n"}
	info, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})

	if err != nil {
		t.Fatalf("Init() unexpected error: %v", err)
	}
	if info.MaxSize != defaultMaxSize {
		t.Errorf("ProviderInfo.MaxSize = %d, want %d", info.MaxSize, defaultMaxSize)
	}
	if !strings.Contains(info.ID, "fi-hel1") {
		t.Errorf("ProviderInfo.ID = %q, expected to contain zone", info.ID)
	}
}
