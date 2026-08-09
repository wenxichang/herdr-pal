package machinereg

import (
	"errors"
	"strings"
	"testing"
)

func TestOperationErrorUsesSafeMessageAndPreservesCauses(t *testing.T) {
	cause := errors.New("sensitive/path")
	err := &OperationError{Kind: ErrRollbackFailed, Stage: "credential_delete", CredentialID: 9, Cause: cause}
	if message := err.Error(); message != "机器凭据回滚失败（credential_id=9）" || strings.Contains(message, "sensitive/path") {
		t.Fatalf("Error()=%q", message)
	}
	if !errors.Is(err, ErrRollbackFailed) || !errors.Is(err, cause) {
		t.Fatalf("Unwrap() did not preserve causes: %v", err)
	}
	credentialID, ok := CredentialIDFromError(err)
	if !ok || credentialID != 9 {
		t.Fatalf("CredentialIDFromError()=%d,%t", credentialID, ok)
	}
}

func TestNilOperationErrorHasStableBehavior(t *testing.T) {
	var err *OperationError
	if err.Error() != "机器注册操作失败" || err.Unwrap() != nil {
		t.Fatalf("nil OperationError=%q unwrap=%#v", err.Error(), err.Unwrap())
	}
	if credentialID, ok := CredentialIDFromError(errors.New("other")); ok || credentialID != 0 {
		t.Fatalf("CredentialIDFromError(other)=%d,%t", credentialID, ok)
	}
}
