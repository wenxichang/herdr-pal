package adminproto

import "testing"

func TestMethodsContainsHPAPVersionOneSurface(t *testing.T) {
	want := []Method{
		MethodServerStatus,
		MethodServerStop,
		MethodServerDebugEnable,
		MethodServerDebugDisable,
		MethodKeyIssue,
		MethodKeyList,
		MethodKeyShow,
		MethodKeyEnable,
		MethodKeyDisable,
		MethodKeyDelete,
		MethodKeySourceList,
		MethodKeySourceAdd,
		MethodKeySourceRemove,
		MethodKeySourceSet,
		MethodConnectionList,
		MethodConnectionShow,
		MethodConnectionDisconnect,
		MethodSessionList,
	}
	if got := Methods(); len(got) != len(want) {
		t.Fatalf("Methods() length = %d, want %d", len(got), len(want))
	} else {
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("Methods()[%d] = %q, want %q", index, got[index], want[index])
			}
			if !IsKnownMethod(got[index]) {
				t.Fatalf("IsKnownMethod(%q) = false", got[index])
			}
		}
	}
	if IsKnownMethod("key.rotate") {
		t.Fatal("IsKnownMethod() accepted unsupported method")
	}
}

func TestErrorCodesContainsHPAPVersionOneSurface(t *testing.T) {
	want := []ErrorCode{
		CodeProtocolInvalidRequest,
		CodeProtocolUnsupportedVersion,
		CodeProtocolUnsupportedMethod,
		CodeProtocolLimitExceeded,
		CodeArgumentInvalid,
		CodeCredentialNotFound,
		CodeCredentialConflict,
		CodeCredentialSourceRequired,
		CodeCredentialSourceInvalid,
		CodeConnectionNotFound,
		CodeServerBusy,
		CodeServerInternal,
	}
	if got := ErrorCodes(); len(got) != len(want) {
		t.Fatalf("ErrorCodes() length = %d, want %d", len(got), len(want))
	} else {
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("ErrorCodes()[%d] = %q, want %q", index, got[index], want[index])
			}
		}
	}
}
