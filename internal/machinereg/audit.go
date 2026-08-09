package machinereg

import (
	"fmt"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/audit"
)

func (service *Service) emitRegistration(action, outcome string, request Request, adminUsername string, credentialID uint64, delivery, errorStage string, cause error, body string) {
	if service == nil || service.auditor == nil {
		return
	}
	attributes := map[string]string{}
	if request.RegistrationID != "" {
		attributes["registration.id"] = request.RegistrationID
	}
	if len(request.AllowedSources) != 0 {
		attributes["registration.sources"] = strings.Join(sourceStrings(request.AllowedSources), ",")
	}
	if adminUsername != "" {
		attributes["admin.username"] = adminUsername
	}
	if credentialID != 0 {
		attributes["credential.id"] = fmt.Sprintf("%d", credentialID)
	}
	if errorStage != "" {
		attributes["error.stage"] = errorStage
	}
	if cause != nil {
		attributes["error.type"] = fmt.Sprintf("%T", cause)
	}
	event, err := audit.PrepareEvent(audit.Event{
		EventName:   audit.EventNameMachineRegistration,
		Timestamp:   service.now().UTC(),
		PrincipalID: request.PrincipalID,
		BotIDHash:   service.botIDHash,
		Action:      action,
		Outcome:     outcome,
		MachineID:   request.MachineID,
		Delivery:    delivery,
		Body:        service.redactor.Redact(body),
		Attributes:  attributes,
	}, service.now().UTC(), nil)
	if err != nil {
		service.logger.Error("机器注册审计事件构造失败", "action", action, "error_type", fmt.Sprintf("%T", err))
		return
	}
	service.auditor.Emit(event)
}
