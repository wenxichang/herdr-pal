package webadmin

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminauth"
	"github.com/wenxichang/herdr-pal/internal/adminservice"
)

const (
	automationPerSecond = 5
	automationPerMinute = 100
)

type automationLimiter struct {
	mu     sync.Mutex
	events map[string][]time.Time
}

func newAutomationLimiter() *automationLimiter {
	return &automationLimiter{events: make(map[string][]time.Time)}
}

func (limiter *automationLimiter) Allow(tokenID string, now time.Time) bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	minuteCutoff := now.Add(-time.Minute)
	secondCutoff := now.Add(-time.Second)
	current := limiter.events[tokenID]
	kept := current[:0]
	secondCount := 0
	for _, event := range current {
		if !event.After(minuteCutoff) {
			continue
		}
		kept = append(kept, event)
		if event.After(secondCutoff) {
			secondCount++
		}
	}
	if secondCount >= automationPerSecond || len(kept) >= automationPerMinute {
		limiter.events[tokenID] = kept
		return false
	}
	limiter.events[tokenID] = append(kept, now)
	return true
}

func (server *Server) registerAutomationRoutes(mux *http.ServeMux) {
	server.handleMethod(mux, "/admin/api/v1/automation/credentials", http.MethodPost, server.automationHandler(http.HandlerFunc(server.automationIssueCredential)))
	server.handleMethod(mux, "/admin/api/v1/automation/credentials/{id}", http.MethodDelete, server.automationHandler(http.HandlerFunc(server.automationDeleteCredential)))
}

func (server *Server) automationHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		identity, ok := server.authenticateAutomation(request)
		if !ok {
			_ = writeAPIError(writer, request, http.StatusUnauthorized, "unauthenticated", "自动化 Token 无效")
			return
		}
		setRequestActor(request, identity.Username)
		setRequestAutomationToken(request, identity.TokenID)
		if !server.automationLimit.Allow(identity.TokenID, server.now().UTC()) {
			_ = writeAPIError(writer, request, http.StatusTooManyRequests, "rate_limited", "自动化请求频率超过限制")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) authenticateAutomation(request *http.Request) (adminauth.AutomationIdentity, bool) {
	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		return adminauth.AutomationIdentity{}, false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return adminauth.AutomationIdentity{}, false
	}
	identity, err := server.auth.VerifyAutomationBearer(parts[1])
	return identity, err == nil
}

func (server *Server) automationIssueCredential(writer http.ResponseWriter, request *http.Request) {
	var input credentialIssueRequest
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return
	}
	var expiresAt *time.Time
	if input.ExpiresAt != nil {
		parsed, err := time.Parse(time.RFC3339, *input.ExpiresAt)
		if err != nil {
			_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_expiry", "expires_at 必须是 RFC3339 时间")
			return
		}
		parsed = parsed.UTC()
		expiresAt = &parsed
	}
	setRequestTarget(request, "machine_id_hash="+shortTargetHash(input.MachineID))
	result, err := server.admin.IssueCredential(adminservice.IssueCredentialInput{
		PrincipalID: input.PrincipalID, MachineID: input.MachineID, Sources: input.Sources, ExpiresAt: expiresAt,
	})
	if err != nil {
		server.writeServiceError(writer, request, err)
		return
	}
	setRequestTarget(request, "credential_id="+strconv.FormatUint(result.Credential.CredentialID, 10))
	_ = writeAPIData(writer, request, http.StatusCreated, result)
}

func (server *Server) automationDeleteCredential(writer http.ResponseWriter, request *http.Request) {
	id, ok := credentialIDFromPath(writer, request)
	if !ok {
		return
	}
	result, err := server.admin.DeleteCredential(id)
	if err != nil {
		server.writeServiceError(writer, request, err)
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, result)
}
