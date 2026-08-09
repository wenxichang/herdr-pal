package machinereg

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/audit"
	"github.com/wenxichang/herdr-pal/internal/credential"
)

func TestServiceAutoIssuesOnlyForPrincipalWithoutAnyRecords(t *testing.T) {
	harness := newServiceHarness(t)
	var delivered KeyDelivery
	result, err := harness.service.Register(context.Background(), RegisterInput{
		PrincipalID: "user-a", MachineID: "office", Sources: []string{"192.168.0.1"},
	}, func(_ context.Context, value KeyDelivery) error {
		delivered = value
		return nil
	})
	if err != nil || result.Disposition != DispositionAutoIssued || delivered.Token == "" || result.CredentialID == 0 {
		t.Fatalf("result=%#v delivery=%#v err=%v", result, delivered, err)
	}
	second, err := harness.service.Register(context.Background(), RegisterInput{
		PrincipalID: "user-a", MachineID: "mobile", Sources: []string{"192.168.0.2"},
	}, nil)
	if err != nil || second.Disposition != DispositionPending || second.Request == nil {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if strings.Contains(strings.Join(harness.auditor.bodies(), "\n"), delivered.Token) {
		t.Fatal("audit events leaked machine key")
	}
}

func TestServiceListPendingReturnsIndependentSnapshot(t *testing.T) {
	harness := newServiceHarness(t)
	request := harness.createPending(t, "user-a", "office", "127.0.0.1")
	listed := harness.service.ListPending()
	if len(listed) != 1 || listed[0].RegistrationID != request.RegistrationID {
		t.Fatalf("ListPending()=%#v", listed)
	}
	listed[0].AllowedSources[0] = "10.0.0.1"
	again := harness.service.ListPending()
	if again[0].AllowedSources[0] != "127.0.0.1" {
		t.Fatalf("ListPending() returned aliased data: %#v", again)
	}
}

func TestServiceTreatsDisabledCredentialAndAnyPendingAsExistingPrincipal(t *testing.T) {
	harness := newServiceHarness(t)
	_, record, err := harness.credentials.Issue("disabled-user", "old", []string{"127.0.0.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.credentials.Disable(record.CredentialID); err != nil {
		t.Fatal(err)
	}
	result, err := harness.service.Register(context.Background(), RegisterInput{
		PrincipalID: "disabled-user", MachineID: "new", Sources: []string{"127.0.0.1"},
	}, nil)
	if err != nil || result.Disposition != DispositionPending {
		t.Fatalf("result=%#v err=%v", result, err)
	}

	harness.createPending(t, "pending-user", "first", "127.0.0.1")
	result, err = harness.service.Register(context.Background(), RegisterInput{
		PrincipalID: "pending-user", MachineID: "second", Sources: []string{"127.0.0.2"},
	}, nil)
	if err != nil || result.Disposition != DispositionPending {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestServiceTreatsExpiredCredentialAsExistingPrincipal(t *testing.T) {
	harness := newServiceHarness(t)
	expiredAt := time.Now().Add(-time.Hour)
	harness.service.credentials = &listedCredentialManager{
		CredentialManager: harness.credentials,
		records: []credential.Record{{
			CredentialID: 10, PrincipalID: "expired-user", MachineID: "old",
			Status: credential.StatusEnabled, ExpiresAt: &expiredAt,
		}},
	}
	result, err := harness.service.Register(context.Background(), RegisterInput{
		PrincipalID: "expired-user", MachineID: "new", Sources: []string{"127.0.0.1"},
	}, nil)
	if err != nil || result.Disposition != DispositionPending || result.Request == nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestServiceRejectsExistingMachineAndKeepsDuplicatePendingSources(t *testing.T) {
	harness := newServiceHarness(t)
	if _, _, err := harness.credentials.Issue("user-a", "office", []string{"127.0.0.1"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.service.Register(context.Background(), RegisterInput{
		PrincipalID: "user-a", MachineID: "office", Sources: []string{"127.0.0.2"},
	}, nil); !errors.Is(err, ErrMachineExists) {
		t.Fatalf("Register(existing) error=%v", err)
	}

	original := harness.createPending(t, "user-b", "office", "127.0.0.1")
	result, err := harness.service.Register(context.Background(), RegisterInput{
		PrincipalID: "user-b", MachineID: "office", Sources: []string{"10.0.0.1"},
	}, nil)
	if err != nil || result.Disposition != DispositionAlreadyPending || result.Request == nil ||
		result.Request.RegistrationID != original.RegistrationID || result.Request.AllowedSources[0] != "127.0.0.1" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestRegisterRollsBackAutoIssuedCredentialWhenDeliveryFails(t *testing.T) {
	harness := newServiceHarness(t)
	_, err := harness.service.Register(context.Background(), RegisterInput{
		PrincipalID: "user-a", MachineID: "office", Sources: []string{"127.0.0.1"},
	}, func(context.Context, KeyDelivery) error { return errors.New("wecom unavailable") })
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("Register() error=%v", err)
	}
	if hasMachine(harness.credentials.List(), "user-a", "office") {
		t.Fatal("credential was not rolled back")
	}
}

func TestRegisterReportsRollbackFailureAndRetainsCredential(t *testing.T) {
	harness := newServiceHarness(t)
	harness.service.credentials = &failingDeleteManager{CredentialManager: harness.credentials, err: errors.New("disk unavailable")}
	_, err := harness.service.Register(context.Background(), RegisterInput{
		PrincipalID: "user-a", MachineID: "office", Sources: []string{"127.0.0.1"},
	}, func(context.Context, KeyDelivery) error { return errors.New("wecom unavailable") })
	if !errors.Is(err, ErrRollbackFailed) {
		t.Fatalf("Register() error=%v", err)
	}
	credentialID, ok := CredentialIDFromError(err)
	if !ok || credentialID == 0 || !hasMachine(harness.credentials.List(), "user-a", "office") {
		t.Fatalf("credential_id=%d ok=%t credentials=%#v", credentialID, ok, harness.credentials.List())
	}
}

func TestRegisterRejectsInvalidMachineBeforeIssuingCredential(t *testing.T) {
	harness := newServiceHarness(t)
	if _, err := harness.service.Register(context.Background(), RegisterInput{
		PrincipalID: "user-a", MachineID: "bad machine", Sources: []string{"127.0.0.1"},
	}, func(context.Context, KeyDelivery) error { return nil }); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Register() error=%v", err)
	}
	if len(harness.credentials.List()) != 0 {
		t.Fatalf("credentials=%#v", harness.credentials.List())
	}
}

func TestApproveRollsBackCredentialAndKeepsRequestWhenDeliveryFails(t *testing.T) {
	harness := newServiceHarness(t)
	request := harness.createPending(t, "user-a", "office", "192.168.0.1")
	_, err := harness.service.Approve(context.Background(), request.RegistrationID, "admin", func(context.Context, KeyDelivery) error {
		return errors.New("wecom unavailable")
	})
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("error=%v", err)
	}
	if _, err := harness.requests.Show(request.RegistrationID); err != nil {
		t.Fatalf("pending removed: %v", err)
	}
	if hasMachine(harness.credentials.List(), "user-a", "office") {
		t.Fatal("credential was not rolled back")
	}
}

func TestApproveSucceedsAndRemovesPendingRequest(t *testing.T) {
	harness := newServiceHarness(t)
	request := harness.createPending(t, "user-a", "office", "192.168.0.1")
	var delivered KeyDelivery
	result, err := harness.service.Approve(context.Background(), request.RegistrationID, "admin", func(_ context.Context, value KeyDelivery) error {
		delivered = value
		return nil
	})
	if err != nil || result.CredentialID == 0 || delivered.Token == "" || result.Request.RegistrationID != request.RegistrationID {
		t.Fatalf("result=%#v delivered=%#v err=%v", result, delivered, err)
	}
	if _, err := harness.requests.Show(request.RegistrationID); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("pending remains: %v", err)
	}
}

func TestApproveRejectsExistingMachineAndKeepsPendingRequest(t *testing.T) {
	harness := newServiceHarness(t)
	request := harness.createPending(t, "user-a", "office", "127.0.0.1")
	if _, _, err := harness.credentials.Issue("user-a", "office", []string{"127.0.0.1"}, nil); err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	_, err := harness.service.Approve(context.Background(), request.RegistrationID, "admin", func(context.Context, KeyDelivery) error {
		deliveries++
		return nil
	})
	if !errors.Is(err, ErrMachineExists) || deliveries != 0 {
		t.Fatalf("Approve() error=%v deliveries=%d", err, deliveries)
	}
	if _, err := harness.requests.Show(request.RegistrationID); err != nil {
		t.Fatalf("pending removed: %v", err)
	}
}

func TestApproveReportsRollbackAndCleanupFailuresWithCredentialID(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*serviceHarness) CredentialManager
		deliver KeyDeliveryFunc
		kind    error
	}{
		{
			name: "rollback",
			prepare: func(harness *serviceHarness) CredentialManager {
				return &failingDeleteManager{CredentialManager: harness.credentials, err: errors.New("disk unavailable")}
			},
			deliver: func(context.Context, KeyDelivery) error { return errors.New("wecom unavailable") },
			kind:    ErrRollbackFailed,
		},
		{
			name: "cleanup",
			prepare: func(harness *serviceHarness) CredentialManager {
				harness.requests.path = filepath.Join(harness.requests.path, "unwritable")
				return harness.credentials
			},
			deliver: func(context.Context, KeyDelivery) error { return nil },
			kind:    ErrCleanupFailed,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newServiceHarness(t)
			request := harness.createPending(t, "user-a", "office", "127.0.0.1")
			harness.service.credentials = test.prepare(harness)
			_, err := harness.service.Approve(context.Background(), request.RegistrationID, "admin", test.deliver)
			if !errors.Is(err, test.kind) {
				t.Fatalf("Approve() error=%v", err)
			}
			if credentialID, ok := CredentialIDFromError(err); !ok || credentialID == 0 {
				t.Fatalf("CredentialIDFromError(%v)=%d,%t", err, credentialID, ok)
			}
			if _, showErr := harness.requests.Show(request.RegistrationID); showErr != nil {
				t.Fatalf("pending removed after %s failure: %v", test.name, showErr)
			}
			if !hasMachine(harness.credentials.List(), "user-a", "office") {
				t.Fatalf("credential missing after %s failure", test.name)
			}
		})
	}
}

func TestRejectDeletesRequestAndNotificationFailureDoesNotRestoreIt(t *testing.T) {
	harness := newServiceHarness(t)
	request := harness.createPending(t, "user-a", "office", "127.0.0.1")
	result, err := harness.service.Reject(context.Background(), request.RegistrationID, "admin", "不符合来源策略", func(context.Context, RejectionDelivery) error {
		return errors.New("wecom unavailable")
	})
	if err != nil || result.NotificationSent {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := harness.requests.Show(request.RegistrationID); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("pending restored: %v", err)
	}
}

func TestRejectValidatesReasonBeforeDeleting(t *testing.T) {
	harness := newServiceHarness(t)
	for index, reason := range []string{"bad\x00reason", strings.Repeat("好", MaxRejectionReasonBytes)} {
		request := harness.createPending(t, "user-"+string(rune('a'+index)), "office", "127.0.0.1")
		if _, err := harness.service.Reject(context.Background(), request.RegistrationID, "admin", reason, nil); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Reject(%q) error=%v", reason[:1], err)
		}
		if _, err := harness.requests.Show(request.RegistrationID); err != nil {
			t.Fatalf("invalid reason removed pending: %v", err)
		}
	}
}

func TestManualIssueConflictsWithPendingAndSerializesWithRegister(t *testing.T) {
	harness := newServiceHarness(t)
	harness.createPending(t, "pending-user", "office", "127.0.0.1")
	if _, _, err := harness.service.Issue("pending-user", "office", []string{"127.0.0.1"}, nil); !errors.Is(err, credential.ErrCredentialConflict) {
		t.Fatalf("Issue(pending) error=%v", err)
	}

	var wait sync.WaitGroup
	wait.Add(2)
	errorsSeen := make(chan error, 2)
	go func() {
		defer wait.Done()
		_, _, err := harness.service.Issue("race-user", "office", []string{"127.0.0.1"}, nil)
		errorsSeen <- err
	}()
	go func() {
		defer wait.Done()
		_, err := harness.service.Register(context.Background(), RegisterInput{
			PrincipalID: "race-user", MachineID: "office", Sources: []string{"127.0.0.1"},
		}, func(context.Context, KeyDelivery) error { return nil })
		errorsSeen <- err
	}()
	wait.Wait()
	close(errorsSeen)
	successes := 0
	for err := range errorsSeen {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, ErrMachineExists) && !errors.Is(err, credential.ErrCredentialConflict) {
			t.Fatalf("unexpected error=%v", err)
		}
	}
	if successes != 1 || countMachine(harness.credentials.List(), "race-user", "office") != 1 {
		t.Fatalf("successes=%d credentials=%#v", successes, harness.credentials.List())
	}
}

func TestServiceCredentialManagerMethodsDelegateThroughCoordinator(t *testing.T) {
	harness := newServiceHarness(t)
	_, issued, err := harness.service.Issue("user-a", "office", []string{"127.0.0.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if listed := harness.service.List(); len(listed) != 1 || listed[0].CredentialID != issued.CredentialID {
		t.Fatalf("List()=%#v", listed)
	}
	if shown, err := harness.service.Show(issued.CredentialID); err != nil || shown.MachineID != "office" {
		t.Fatalf("Show()=%#v err=%v", shown, err)
	}
	if disabled, err := harness.service.Disable(issued.CredentialID); err != nil || disabled.Status != credential.StatusDisabled {
		t.Fatalf("Disable()=%#v err=%v", disabled, err)
	}
	if enabled, err := harness.service.Enable(issued.CredentialID); err != nil || enabled.Status != credential.StatusEnabled {
		t.Fatalf("Enable()=%#v err=%v", enabled, err)
	}
	if added, err := harness.service.AddSources(issued.CredentialID, []string{"10.0.0.1"}); err != nil || len(added.AllowedSources) != 2 {
		t.Fatalf("AddSources()=%#v err=%v", added, err)
	}
	if removed, err := harness.service.RemoveSources(issued.CredentialID, []string{"127.0.0.1"}); err != nil || len(removed.AllowedSources) != 1 || removed.AllowedSources[0] != "10.0.0.1" {
		t.Fatalf("RemoveSources()=%#v err=%v", removed, err)
	}
	if replaced, err := harness.service.SetSources(issued.CredentialID, []string{"192.168.0.1"}); err != nil || len(replaced.AllowedSources) != 1 || replaced.AllowedSources[0] != "192.168.0.1" {
		t.Fatalf("SetSources()=%#v err=%v", replaced, err)
	}
	if deleted, err := harness.service.Delete(issued.CredentialID); err != nil || deleted.CredentialID != issued.CredentialID || len(harness.service.List()) != 0 {
		t.Fatalf("Delete()=%#v err=%v list=%#v", deleted, err, harness.service.List())
	}
}

func TestServiceCredentialMutationRevalidatesPrincipalUnderLock(t *testing.T) {
	harness := newServiceHarness(t)
	manager := &changingPrincipalManager{CredentialManager: harness.credentials, record: credential.Record{
		CredentialID: 1, PrincipalID: "user-a", MachineID: "office",
	}}
	harness.service.credentials = manager
	if _, err := harness.service.Disable(1); !errors.Is(err, credential.ErrCredentialConflict) {
		t.Fatalf("Disable() error=%v", err)
	}
	if manager.disableCalls.Load() != 0 {
		t.Fatalf("Disable() delegated after principal changed: %d", manager.disableCalls.Load())
	}
}

func TestServiceCredentialMutationSerializesRegistrationForSamePrincipal(t *testing.T) {
	harness := newServiceHarness(t)
	_, record, err := harness.credentials.Issue("user-a", "old", []string{"127.0.0.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := &blockingDisableManager{
		CredentialManager: harness.credentials,
		disableEntered:    make(chan struct{}),
		releaseDisable:    make(chan struct{}),
		listCalled:        make(chan struct{}),
	}
	harness.service.credentials = manager
	disableResult := make(chan error, 1)
	go func() {
		_, err := harness.service.Disable(record.CredentialID)
		disableResult <- err
	}()
	<-manager.disableEntered
	registerStarted := make(chan struct{})
	registerResult := make(chan error, 1)
	go func() {
		close(registerStarted)
		_, err := harness.service.Register(context.Background(), RegisterInput{
			PrincipalID: "user-a", MachineID: "new", Sources: []string{"127.0.0.2"},
		}, nil)
		registerResult <- err
	}()
	<-registerStarted
	select {
	case <-manager.listCalled:
		t.Fatal("Register() crossed an in-progress credential mutation")
	case <-time.After(50 * time.Millisecond):
	}
	close(manager.releaseDisable)
	if err := <-disableResult; err != nil {
		t.Fatal(err)
	}
	if err := <-registerResult; err != nil {
		t.Fatal(err)
	}
	if result := harness.requests.List(); len(result) != 1 || result[0].MachineID != "new" {
		t.Fatalf("pending=%#v", result)
	}
}

func TestConcurrentApprovalDeliversOnlyOneCredential(t *testing.T) {
	harness := newServiceHarness(t)
	request := harness.createPending(t, "user-a", "office", "127.0.0.1")
	var deliveries atomic.Int32
	errorsSeen := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	for range 2 {
		go func() {
			defer wait.Done()
			_, err := harness.service.Approve(context.Background(), request.RegistrationID, "admin", func(context.Context, KeyDelivery) error {
				deliveries.Add(1)
				return nil
			})
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	successes := 0
	notFound := 0
	for err := range errorsSeen {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrRequestNotFound):
			notFound++
		default:
			t.Fatalf("unexpected error=%v", err)
		}
	}
	if successes != 1 || notFound != 1 || deliveries.Load() != 1 || countMachine(harness.credentials.List(), "user-a", "office") != 1 {
		t.Fatalf("successes=%d not_found=%d deliveries=%d credentials=%#v", successes, notFound, deliveries.Load(), harness.credentials.List())
	}
}

func TestConcurrentApprovalAndRejectionHaveSingleWinner(t *testing.T) {
	harness := newServiceHarness(t)
	request := harness.createPending(t, "user-a", "office", "127.0.0.1")
	var deliveries atomic.Int32
	errorsSeen := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, err := harness.service.Approve(context.Background(), request.RegistrationID, "admin", func(context.Context, KeyDelivery) error {
			deliveries.Add(1)
			return nil
		})
		errorsSeen <- err
	}()
	go func() {
		defer wait.Done()
		_, err := harness.service.Reject(context.Background(), request.RegistrationID, "admin", "重复申请", nil)
		errorsSeen <- err
	}()
	wait.Wait()
	close(errorsSeen)
	successes := 0
	notFound := 0
	for err := range errorsSeen {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrRequestNotFound):
			notFound++
		default:
			t.Fatalf("unexpected error=%v", err)
		}
	}
	if successes != 1 || notFound != 1 || len(harness.requests.List()) != 0 || int(deliveries.Load()) != countMachine(harness.credentials.List(), "user-a", "office") {
		t.Fatalf("successes=%d not_found=%d deliveries=%d pending=%#v credentials=%#v", successes, notFound, deliveries.Load(), harness.requests.List(), harness.credentials.List())
	}
}

func TestServiceEmitsRegistrationLifecycleAuditAttributes(t *testing.T) {
	harness := newServiceHarness(t)
	if _, _, err := harness.credentials.Issue("user-a", "old", []string{"127.0.0.1"}, nil); err != nil {
		t.Fatal(err)
	}
	result, err := harness.service.Register(context.Background(), RegisterInput{
		PrincipalID: "user-a", MachineID: "office", Sources: []string{"192.168.0.1"},
	}, nil)
	if err != nil || result.Request == nil {
		t.Fatalf("Register() result=%#v err=%v", result, err)
	}
	if _, err := harness.service.Approve(context.Background(), result.Request.RegistrationID, "admin-a", func(context.Context, KeyDelivery) error { return nil }); err != nil {
		t.Fatal(err)
	}
	rejected := harness.createPending(t, "user-b", "mobile", "10.0.0.1")
	if _, err := harness.service.Reject(context.Background(), rejected.RegistrationID, "admin-b", "来源不符", func(context.Context, RejectionDelivery) error {
		return errors.New("wecom unavailable")
	}); err != nil {
		t.Fatal(err)
	}
	events := harness.auditor.snapshot()
	approved := findRegistrationEvent(events, "approve", "delivered")
	if approved == nil || approved.Attributes["registration.sources"] != "192.168.0.1" || approved.Attributes["admin.username"] != "admin-a" || approved.Attributes["credential.id"] == "" {
		t.Fatalf("approve audit=%#v", approved)
	}
	rejection := findRegistrationEvent(events, "reject", "rejected")
	if rejection == nil || rejection.Delivery != "failed" || rejection.Attributes["admin.username"] != "admin-b" || rejection.Attributes["error.stage"] != "rejection_delivery" || rejection.Attributes["error.type"] == "" {
		t.Fatalf("reject audit=%#v", rejection)
	}
}

type serviceHarness struct {
	service     *Service
	credentials *credential.Store
	requests    *Store
	auditor     *recordingAuditor
}

func newServiceHarness(t *testing.T) *serviceHarness {
	t.Helper()
	credentials, err := credential.LoadStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	requests, err := LoadStore(filepath.Join(t.TempDir(), "registrations.json"), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	auditor := &recordingAuditor{}
	service, err := New(Config{
		Credentials: credentials,
		Requests:    requests,
		Auditor:     auditor,
		Redactor:    audit.NewRedactor(nil),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		BotIDHash:   "bot-hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &serviceHarness{service: service, credentials: credentials, requests: requests, auditor: auditor}
}

func (harness *serviceHarness) createPending(t *testing.T, principalID, machineID string, sources ...string) Request {
	t.Helper()
	rules, err := credential.NormalizeSourceRules(sources)
	if err != nil {
		t.Fatal(err)
	}
	request, _, err := harness.requests.Create(principalID, machineID, rules)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func hasMachine(records []credential.Record, principalID, machineID string) bool {
	return countMachine(records, principalID, machineID) != 0
}

func countMachine(records []credential.Record, principalID, machineID string) int {
	count := 0
	for _, record := range records {
		if record.PrincipalID == principalID && record.MachineID == machineID {
			count++
		}
	}
	return count
}

type failingDeleteManager struct {
	CredentialManager
	err error
}

type listedCredentialManager struct {
	CredentialManager
	records []credential.Record
}

func (manager *listedCredentialManager) List() []credential.Record {
	return append([]credential.Record(nil), manager.records...)
}

type changingPrincipalManager struct {
	CredentialManager
	record       credential.Record
	showCalls    atomic.Int32
	disableCalls atomic.Int32
}

func (manager *changingPrincipalManager) Show(uint64) (credential.Record, error) {
	record := manager.record
	if manager.showCalls.Add(1) > 1 {
		record.PrincipalID = "user-b"
	}
	return record, nil
}

func (manager *changingPrincipalManager) Disable(uint64) (credential.Record, error) {
	manager.disableCalls.Add(1)
	return manager.record, nil
}

type blockingDisableManager struct {
	CredentialManager
	disableEntered chan struct{}
	releaseDisable chan struct{}
	listCalled     chan struct{}
	listOnce       sync.Once
}

func (manager *blockingDisableManager) Disable(id uint64) (credential.Record, error) {
	close(manager.disableEntered)
	<-manager.releaseDisable
	return manager.CredentialManager.Disable(id)
}

func (manager *blockingDisableManager) List() []credential.Record {
	manager.listOnce.Do(func() { close(manager.listCalled) })
	return manager.CredentialManager.List()
}

func (manager *failingDeleteManager) Delete(uint64) (credential.Record, error) {
	return credential.Record{}, manager.err
}

type recordingAuditor struct {
	mu     sync.Mutex
	events []audit.Event
}

func (auditor *recordingAuditor) Emit(event audit.Event) {
	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	auditor.events = append(auditor.events, event)
}

func (auditor *recordingAuditor) Shutdown(context.Context) error { return nil }

func (auditor *recordingAuditor) bodies() []string {
	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	result := make([]string, len(auditor.events))
	for index, event := range auditor.events {
		result[index] = event.Body
	}
	return result
}

func (auditor *recordingAuditor) snapshot() []audit.Event {
	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	return append([]audit.Event(nil), auditor.events...)
}

func findRegistrationEvent(events []audit.Event, action, outcome string) *audit.Event {
	for index := range events {
		if events[index].Action == action && events[index].Outcome == outcome {
			return &events[index]
		}
	}
	return nil
}
