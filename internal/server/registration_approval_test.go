package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/machinereg"
)

func TestRegistrationApprovalCoordinatorNotifiesAdminsRoundRobin(t *testing.T) {
	manager := newFakeRegistrationApprovalManager()
	gateway := newFakeRegistrationApprovalGateway()
	coordinator := newTestRegistrationApprovalCoordinator(t, manager, gateway, "admin-a", "admin-b")

	for index, machineID := range []string{"office", "lab", "mobile"} {
		request := approvalRequest("reg-"+machineID, "user-a", machineID, time.Date(2026, 8, 10, index, 0, 0, 0, time.UTC))
		if err := coordinator.NotifyPending(context.Background(), request); err != nil {
			t.Fatalf("NotifyPending(%q) error = %v", machineID, err)
		}
	}

	messages := gateway.sentMessages()
	wantUsers := []string{"admin-a", "admin-b", "admin-a"}
	if len(messages) != len(wantUsers) {
		t.Fatalf("sent messages = %#v", messages)
	}
	for index, message := range messages {
		if message.userID != wantUsers[index] {
			t.Fatalf("message %d user = %q, want %q", index, message.userID, wantUsers[index])
		}
		if !strings.Contains(message.content, []string{"office", "lab", "mobile"}[index]) || !strings.Contains(message.content, "/ls-reg") {
			t.Fatalf("message %d content = %q", index, message.content)
		}
	}
}

func TestRegistrationApprovalCoordinatorNotificationFailureDoesNotRetryAdmin(t *testing.T) {
	manager := newFakeRegistrationApprovalManager()
	gateway := newFakeRegistrationApprovalGateway()
	gateway.failures["admin-a"] = 1
	coordinator := newTestRegistrationApprovalCoordinator(t, manager, gateway, "admin-a", "admin-b")

	if err := coordinator.NotifyPending(context.Background(), approvalRequest("reg-a", "user-a", "office", time.Now())); err == nil {
		t.Fatal("NotifyPending() error = nil, want delivery failure")
	}
	if err := coordinator.NotifyPending(context.Background(), approvalRequest("reg-b", "user-a", "lab", time.Now())); err != nil {
		t.Fatalf("second NotifyPending() error = %v", err)
	}

	messages := gateway.sentMessages()
	if got := messageUsers(messages); !reflect.DeepEqual(got, []string{"admin-a", "admin-b"}) {
		t.Fatalf("notification attempts = %#v, want no retry", got)
	}
}

func TestRegistrationApprovalCoordinatorDoesNotNotifyWithoutAdmins(t *testing.T) {
	manager := newFakeRegistrationApprovalManager()
	gateway := newFakeRegistrationApprovalGateway()
	coordinator := newTestRegistrationApprovalCoordinator(t, manager, gateway)

	if err := coordinator.NotifyPending(context.Background(), approvalRequest("reg-a", "user-a", "office", time.Now())); err != nil {
		t.Fatalf("NotifyPending() error = %v", err)
	}
	if messages := gateway.sentMessages(); len(messages) != 0 {
		t.Fatalf("sent messages = %#v, want none", messages)
	}
}

func TestRegistrationApprovalCoordinatorListStoresPrivateSnapshots(t *testing.T) {
	manager := newFakeRegistrationApprovalManager(
		approvalRequest("reg-a", "user-a", "office", time.Date(2026, 8, 10, 8, 0, 0, 0, time.FixedZone("CST", 8*60*60))),
		approvalRequest("reg-b", "user-b", "lab", time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC)),
	)
	coordinator := newTestRegistrationApprovalCoordinator(t, manager, newFakeRegistrationApprovalGateway(), "admin-a", "admin-b")

	content, err := coordinator.List("admin-a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "1. 用户：user-a") || !strings.Contains(content, "机器：office") ||
		!strings.Contains(content, "来源：127.0.0.1") || !strings.Contains(content, "2026-08-10T00:00:00Z") {
		t.Fatalf("List() content = %q", content)
	}
	if _, err := coordinator.List("admin-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Approve(context.Background(), "admin-a", []int{2}); err != nil {
		t.Fatalf("admin-a Approve() error = %v", err)
	}
	if _, err := coordinator.Approve(context.Background(), "admin-b", []int{1}); err != nil {
		t.Fatalf("admin-b Approve() error = %v", err)
	}
	if got := manager.approvedIDs(); !reflect.DeepEqual(got, []string{"reg-b", "reg-a"}) {
		t.Fatalf("approved IDs = %#v", got)
	}
}

func TestRegistrationApprovalCoordinatorAllowsGlobalListGrowth(t *testing.T) {
	manager := newFakeRegistrationApprovalManager(approvalRequest("reg-a", "user-a", "office", time.Now()))
	coordinator := newTestRegistrationApprovalCoordinator(t, manager, newFakeRegistrationApprovalGateway(), "admin-a")

	if _, err := coordinator.List("admin-a"); err != nil {
		t.Fatal(err)
	}
	manager.appendPending(approvalRequest("reg-b", "user-b", "lab", time.Now().Add(time.Second)))
	if _, err := coordinator.Approve(context.Background(), "admin-a", []int{1}); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if got := manager.approvedIDs(); !reflect.DeepEqual(got, []string{"reg-a"}) {
		t.Fatalf("approved IDs = %#v", got)
	}
}

func TestRegistrationApprovalCoordinatorRejectsChangedSelectedPosition(t *testing.T) {
	manager := newFakeRegistrationApprovalManager(
		approvalRequest("reg-a", "user-a", "office", time.Now()),
		approvalRequest("reg-b", "user-b", "lab", time.Now().Add(time.Second)),
	)
	coordinator := newTestRegistrationApprovalCoordinator(t, manager, newFakeRegistrationApprovalGateway(), "admin-a")

	if _, err := coordinator.List("admin-a"); err != nil {
		t.Fatal(err)
	}
	manager.setPending(approvalRequest("reg-b", "user-b", "lab", time.Now()))
	_, err := coordinator.Approve(context.Background(), "admin-a", []int{1})
	if !errors.Is(err, ErrRegistrationApprovalSnapshotChanged) {
		t.Fatalf("Approve() error = %v, want snapshot changed", err)
	}
	if got := manager.approvedIDs(); len(got) != 0 {
		t.Fatalf("approved IDs = %#v, want none", got)
	}
}

func TestRegistrationApprovalCoordinatorInvalidatesSnapshotAfterAttempt(t *testing.T) {
	manager := newFakeRegistrationApprovalManager(approvalRequest("reg-a", "user-a", "office", time.Now()))
	manager.approveErrors["reg-a"] = machinereg.ErrDeliveryFailed
	coordinator := newTestRegistrationApprovalCoordinator(t, manager, newFakeRegistrationApprovalGateway(), "admin-a")

	if _, err := coordinator.List("admin-a"); err != nil {
		t.Fatal(err)
	}
	content, err := coordinator.Approve(context.Background(), "admin-a", []int{1})
	if err != nil {
		t.Fatalf("first Approve() error = %v", err)
	}
	if !strings.Contains(content, "Key 交付失败") || !strings.Contains(content, registrationApprovalSnapshotReminder) {
		t.Fatalf("first Approve() content = %q", content)
	}
	_, err = coordinator.Approve(context.Background(), "admin-a", []int{1})
	if !errors.Is(err, ErrRegistrationApprovalSnapshotMissing) {
		t.Fatalf("second Approve() error = %v, want missing snapshot", err)
	}
}

func TestRegistrationApprovalCoordinatorInvalidatesSnapshotExplicitly(t *testing.T) {
	manager := newFakeRegistrationApprovalManager(approvalRequest("reg-a", "user-a", "office", time.Now()))
	coordinator := newTestRegistrationApprovalCoordinator(t, manager, newFakeRegistrationApprovalGateway(), "admin-a")

	if _, err := coordinator.List("admin-a"); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Invalidate("admin-a"); err != nil {
		t.Fatalf("Invalidate() error = %v", err)
	}
	if _, err := coordinator.Approve(context.Background(), "admin-a", []int{1}); !errors.Is(err, ErrRegistrationApprovalSnapshotMissing) {
		t.Fatalf("Approve() error = %v, want missing snapshot", err)
	}
}

func TestRegistrationApprovalCoordinatorContinuesBatchAfterItemFailure(t *testing.T) {
	manager := newFakeRegistrationApprovalManager(
		approvalRequest("reg-a", "user-a", "office", time.Now()),
		approvalRequest("reg-b", "user-b", "lab", time.Now().Add(time.Second)),
		approvalRequest("reg-c", "user-c", "mobile", time.Now().Add(2*time.Second)),
	)
	manager.approveErrors["reg-b"] = machinereg.ErrDeliveryFailed
	coordinator := newTestRegistrationApprovalCoordinator(t, manager, newFakeRegistrationApprovalGateway(), "admin-a")

	if _, err := coordinator.List("admin-a"); err != nil {
		t.Fatal(err)
	}
	content, err := coordinator.Approve(context.Background(), "admin-a", []int{1, 2, 3})
	if err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if got := manager.approveAttemptIDs(); !reflect.DeepEqual(got, []string{"reg-a", "reg-b", "reg-c"}) {
		t.Fatalf("approve attempts = %#v", got)
	}
	if got := manager.approvedIDs(); !reflect.DeepEqual(got, []string{"reg-a", "reg-c"}) {
		t.Fatalf("approved IDs = %#v", got)
	}
	for _, want := range []string{"1. office：已批准", "2. lab：批准失败，Key 交付失败", "3. mobile：已批准", registrationApprovalSnapshotReminder} {
		if !strings.Contains(content, want) {
			t.Fatalf("Approve() content = %q, want %q", content, want)
		}
	}
}

func TestRegistrationApprovalCoordinatorRejectsRegistrations(t *testing.T) {
	manager := newFakeRegistrationApprovalManager(
		approvalRequest("reg-a", "user-a", "office", time.Now()),
		approvalRequest("reg-b", "user-b", "lab", time.Now().Add(time.Second)),
	)
	manager.rejectionNotificationSent["reg-b"] = false
	coordinator := newTestRegistrationApprovalCoordinator(t, manager, newFakeRegistrationApprovalGateway(), "admin-a")

	if _, err := coordinator.List("admin-a"); err != nil {
		t.Fatal(err)
	}
	content, err := coordinator.Reject(context.Background(), "admin-a", []int{1, 2})
	if err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	if got := manager.rejectedIDs(); !reflect.DeepEqual(got, []string{"reg-a", "reg-b"}) {
		t.Fatalf("rejected IDs = %#v", got)
	}
	if actors := manager.rejectActors(); !reflect.DeepEqual(actors, []string{"wecom:admin-a", "wecom:admin-a"}) {
		t.Fatalf("reject actors = %#v", actors)
	}
	if !strings.Contains(content, "1. office：已驳回，已通知申请人") || !strings.Contains(content, "2. lab：已驳回，但申请人通知发送失败") {
		t.Fatalf("Reject() content = %q", content)
	}
}

func TestRegistrationApprovalCoordinatorConcurrentSnapshotsDoNotApproveShiftedItem(t *testing.T) {
	manager := newFakeRegistrationApprovalManager(
		approvalRequest("reg-a", "user-a", "office", time.Now()),
		approvalRequest("reg-b", "user-b", "lab", time.Now().Add(time.Second)),
	)
	coordinator := newTestRegistrationApprovalCoordinator(t, manager, newFakeRegistrationApprovalGateway(), "admin-a", "admin-b")

	if _, err := coordinator.List("admin-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.List("admin-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Approve(context.Background(), "admin-a", []int{1}); err != nil {
		t.Fatal(err)
	}
	_, err := coordinator.Approve(context.Background(), "admin-b", []int{1})
	if !errors.Is(err, ErrRegistrationApprovalSnapshotChanged) {
		t.Fatalf("Approve() error = %v, want snapshot changed", err)
	}
	if got := manager.approvedIDs(); !reflect.DeepEqual(got, []string{"reg-a"}) {
		t.Fatalf("approved IDs = %#v", got)
	}
}

func TestRegistrationApprovalCoordinatorRejectsUnauthorizedAdmin(t *testing.T) {
	coordinator := newTestRegistrationApprovalCoordinator(t, newFakeRegistrationApprovalManager(), newFakeRegistrationApprovalGateway(), "admin-a")
	if coordinator.IsAdmin("other") {
		t.Fatal("IsAdmin(other) = true")
	}
	if _, err := coordinator.List("other"); !errors.Is(err, ErrRegistrationApprovalUnauthorized) {
		t.Fatalf("List(other) error = %v", err)
	}
}

func TestNewRegistrationApprovalCoordinatorRejectsInvalidAdminIDs(t *testing.T) {
	for _, adminIDs := range [][]string{
		{" admin-a "},
		{"admin\nroot"},
		{"admin-a", "admin-a"},
	} {
		_, err := NewRegistrationApprovalCoordinator(RegistrationApprovalCoordinatorConfig{
			AdminIDs:      adminIDs,
			Registrations: newFakeRegistrationApprovalManager(),
			Gateway:       newFakeRegistrationApprovalGateway(),
			KeyDelivery:   func(context.Context, machinereg.KeyDelivery) error { return nil },
		})
		if !errors.Is(err, ErrInvalidRegistrationApprovalDependency) {
			t.Fatalf("admin IDs %#v error = %v", adminIDs, err)
		}
	}
}

func newTestRegistrationApprovalCoordinator(t *testing.T, manager RegistrationApprovalManager, gateway WeComGateway, adminIDs ...string) *RegistrationApprovalCoordinator {
	t.Helper()
	coordinator, err := NewRegistrationApprovalCoordinator(RegistrationApprovalCoordinatorConfig{
		AdminIDs:          adminIDs,
		Registrations:     manager,
		Gateway:           gateway,
		KeyDelivery:       func(context.Context, machinereg.KeyDelivery) error { return nil },
		RejectionDelivery: func(context.Context, machinereg.RejectionDelivery) error { return nil },
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewRegistrationApprovalCoordinator() error = %v", err)
	}
	return coordinator
}

func approvalRequest(registrationID, principalID, machineID string, requestedAt time.Time) machinereg.Request {
	return machinereg.Request{
		RegistrationID: registrationID,
		PrincipalID:    principalID,
		MachineID:      machineID,
		AllowedSources: []credential.SourceRule{"127.0.0.1"},
		RequestedAt:    requestedAt,
	}
}

type fakeRegistrationApprovalManager struct {
	mu                        sync.Mutex
	pending                   []machinereg.Request
	approveErrors             map[string]error
	rejectErrors              map[string]error
	rejectionNotificationSent map[string]bool
	approveAttempts           []registrationApprovalCall
	approveSuccesses          []string
	rejectAttempts            []registrationApprovalCall
	rejectSuccesses           []string
}

type registrationApprovalCall struct {
	registrationID string
	actor          string
}

func newFakeRegistrationApprovalManager(requests ...machinereg.Request) *fakeRegistrationApprovalManager {
	notificationSent := make(map[string]bool, len(requests))
	for _, request := range requests {
		notificationSent[request.RegistrationID] = true
	}
	return &fakeRegistrationApprovalManager{
		pending:                   append([]machinereg.Request(nil), requests...),
		approveErrors:             make(map[string]error),
		rejectErrors:              make(map[string]error),
		rejectionNotificationSent: notificationSent,
	}
}

func (manager *fakeRegistrationApprovalManager) ListPending() []machinereg.Request {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return append([]machinereg.Request(nil), manager.pending...)
}

func (manager *fakeRegistrationApprovalManager) Approve(_ context.Context, registrationID, actor string, _ machinereg.KeyDeliveryFunc) (machinereg.ApprovalResult, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.approveAttempts = append(manager.approveAttempts, registrationApprovalCall{registrationID: registrationID, actor: actor})
	if err := manager.approveErrors[registrationID]; err != nil {
		return machinereg.ApprovalResult{}, err
	}
	request, found := manager.removePending(registrationID)
	if !found {
		return machinereg.ApprovalResult{}, machinereg.ErrRequestNotFound
	}
	manager.approveSuccesses = append(manager.approveSuccesses, registrationID)
	return machinereg.ApprovalResult{Request: request, CredentialID: uint64(len(manager.approveSuccesses))}, nil
}

func (manager *fakeRegistrationApprovalManager) Reject(_ context.Context, registrationID, actor, _ string, _ machinereg.RejectionDeliveryFunc) (machinereg.RejectionResult, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.rejectAttempts = append(manager.rejectAttempts, registrationApprovalCall{registrationID: registrationID, actor: actor})
	if err := manager.rejectErrors[registrationID]; err != nil {
		return machinereg.RejectionResult{}, err
	}
	request, found := manager.removePending(registrationID)
	if !found {
		return machinereg.RejectionResult{}, machinereg.ErrRequestNotFound
	}
	manager.rejectSuccesses = append(manager.rejectSuccesses, registrationID)
	return machinereg.RejectionResult{Request: request, NotificationSent: manager.rejectionNotificationSent[registrationID]}, nil
}

func (manager *fakeRegistrationApprovalManager) removePending(registrationID string) (machinereg.Request, bool) {
	for index, request := range manager.pending {
		if request.RegistrationID == registrationID {
			manager.pending = append(manager.pending[:index], manager.pending[index+1:]...)
			return request, true
		}
	}
	return machinereg.Request{}, false
}

func (manager *fakeRegistrationApprovalManager) appendPending(request machinereg.Request) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.pending = append(manager.pending, request)
	manager.rejectionNotificationSent[request.RegistrationID] = true
}

func (manager *fakeRegistrationApprovalManager) setPending(requests ...machinereg.Request) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.pending = append([]machinereg.Request(nil), requests...)
}

func (manager *fakeRegistrationApprovalManager) approvedIDs() []string {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return append([]string(nil), manager.approveSuccesses...)
}

func (manager *fakeRegistrationApprovalManager) approveAttemptIDs() []string {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	result := make([]string, len(manager.approveAttempts))
	for index, call := range manager.approveAttempts {
		result[index] = call.registrationID
	}
	return result
}

func (manager *fakeRegistrationApprovalManager) rejectedIDs() []string {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return append([]string(nil), manager.rejectSuccesses...)
}

func (manager *fakeRegistrationApprovalManager) rejectActors() []string {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	result := make([]string, len(manager.rejectAttempts))
	for index, call := range manager.rejectAttempts {
		result[index] = call.actor
	}
	return result
}

type registrationApprovalGatewayMessage struct {
	userID  string
	content string
}

type fakeRegistrationApprovalGateway struct {
	mu       sync.Mutex
	messages []registrationApprovalGatewayMessage
	failures map[string]int
}

func newFakeRegistrationApprovalGateway() *fakeRegistrationApprovalGateway {
	return &fakeRegistrationApprovalGateway{failures: make(map[string]int)}
}

func (*fakeRegistrationApprovalGateway) RespondMarkdown(context.Context, string, string) error {
	return nil
}

func (gateway *fakeRegistrationApprovalGateway) SendMarkdownTo(_ context.Context, userID, content string) error {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	gateway.messages = append(gateway.messages, registrationApprovalGatewayMessage{userID: userID, content: content})
	if gateway.failures[userID] > 0 {
		gateway.failures[userID]--
		return errors.New("send failed")
	}
	return nil
}

func (gateway *fakeRegistrationApprovalGateway) sentMessages() []registrationApprovalGatewayMessage {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	return append([]registrationApprovalGatewayMessage(nil), gateway.messages...)
}

func messageUsers(messages []registrationApprovalGatewayMessage) []string {
	result := make([]string, len(messages))
	for index, message := range messages {
		result[index] = message.userID
	}
	return result
}
