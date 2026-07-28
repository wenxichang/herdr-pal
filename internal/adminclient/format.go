package adminclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

// FormatJSON 把结果写成单个带换行的 JSON document。
func FormatJSON(writer io.Writer, value any) error {
	if writer == nil {
		return errors.New("JSON 输出不可用")
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

// FormatHuman 根据 HPAP 方法写出稳定的人工可读结果。
func FormatHuman(writer io.Writer, method adminproto.Method, value any) error {
	if writer == nil {
		return errors.New("人工输出不可用")
	}
	switch method {
	case adminproto.MethodServerStatus:
		result, ok := value.(adminproto.ServerStatusResult)
		if !ok {
			return formatTypeError(method)
		}
		_, err := fmt.Fprintf(writer,
			"版本：%s（%s）\nPID：%d\n平台：%s/%s\n运行时间：%s\nHPAP/HPRP：%s / %s\nRelay：%s\nAdmin Socket：%s\n企业微信：%s\nTLS：%s，到期 %s\nDebug：%t（基础级别 %s）\nPrincipal/连接/会话：%d / %d / %d\nKey enabled/disabled/expired：%d / %d / %d\n",
			safeHuman(result.Version), safeHuman(result.Commit), result.PID, safeHuman(result.GOOS), safeHuman(result.GOARCH),
			(time.Duration(result.UptimeMS) * time.Millisecond).Round(time.Second), safeHuman(result.HPAP), safeHuman(result.HPRP),
			safeHuman(result.RelayListen), safeHuman(result.AdminSocket), safeHuman(result.WeCom.State), safeHuman(result.TLS.Mode), formatTime(result.TLS.NotAfter),
			result.DebugEnabled, safeHuman(result.BaseLogLevel), result.PrincipalCount, result.ConnectionCount, result.SessionCount,
			result.Credentials.Enabled, result.Credentials.Disabled, result.Credentials.Expired,
		)
		return err
	case adminproto.MethodServerStop:
		result, ok := value.(adminproto.ServerStopResult)
		if !ok {
			return formatTypeError(method)
		}
		if result.Stopping {
			_, err := fmt.Fprintln(writer, "服务端正在停止。")
			return err
		}
	case adminproto.MethodServerDebugEnable, adminproto.MethodServerDebugDisable:
		result, ok := value.(adminproto.ServerDebugResult)
		if !ok {
			return formatTypeError(method)
		}
		_, err := fmt.Fprintf(writer, "Debug：%t（基础级别 %s）\n", result.DebugEnabled, safeHuman(result.BaseLogLevel))
		return err
	case adminproto.MethodKeyIssue:
		result, ok := value.(adminproto.KeyIssueResult)
		if !ok {
			return formatTypeError(method)
		}
		if _, err := fmt.Fprintf(writer, "Key（仅显示一次）：%s\n", result.Token); err != nil {
			return err
		}
		return writeCredential(writer, result.Credential)
	case adminproto.MethodKeyList:
		result, ok := value.(adminproto.KeyListResult)
		if !ok {
			return formatTypeError(method)
		}
		return writeCredentialTable(writer, result.Items)
	case adminproto.MethodKeyShow:
		result, ok := value.(adminproto.CredentialResult)
		if !ok {
			return formatTypeError(method)
		}
		return writeCredential(writer, result.Credential)
	case adminproto.MethodKeyEnable, adminproto.MethodKeyDisable, adminproto.MethodKeySourceAdd, adminproto.MethodKeySourceRemove, adminproto.MethodKeySourceSet:
		result, ok := value.(adminproto.CredentialMutationResult)
		if !ok {
			return formatTypeError(method)
		}
		if err := writeCredential(writer, result.Credential); err != nil {
			return err
		}
		_, err := fmt.Fprintf(writer, "撤下连接：%d\n", result.DisconnectedConnections)
		return err
	case adminproto.MethodKeyDelete:
		result, ok := value.(adminproto.KeyDeleteResult)
		if !ok {
			return formatTypeError(method)
		}
		_, err := fmt.Fprintf(writer, "已删除 Key %d，撤下连接 %d。\n", result.CredentialID, result.DisconnectedConnections)
		return err
	case adminproto.MethodKeySourceList:
		result, ok := value.(adminproto.KeySourceListResult)
		if !ok {
			return formatTypeError(method)
		}
		_, err := fmt.Fprintf(writer, "Key %d 来源：\n%s\n", result.CredentialID, strings.Join(safeHumanSlice(result.Sources), "\n"))
		return err
	case adminproto.MethodConnectionList:
		result, ok := value.(adminproto.ConnectionListResult)
		if !ok {
			return formatTypeError(method)
		}
		return writeConnectionTable(writer, result.Items)
	case adminproto.MethodConnectionShow:
		result, ok := value.(adminproto.ConnectionResult)
		if !ok {
			return formatTypeError(method)
		}
		return writeConnection(writer, result.Connection)
	case adminproto.MethodConnectionDisconnect:
		result, ok := value.(adminproto.ConnectionDisconnectResult)
		if !ok {
			return formatTypeError(method)
		}
		_, err := fmt.Fprintf(writer, "已断开连接 %s。\n", safeHuman(result.ConnectionID))
		return err
	case adminproto.MethodSessionList:
		result, ok := value.(adminproto.SessionListResult)
		if !ok {
			return formatTypeError(method)
		}
		return writeSessionTable(writer, result.Items)
	}
	return formatTypeError(method)
}

func writeCredentialTable(writer io.Writer, items []adminproto.Credential) error {
	if len(items) == 0 {
		_, err := fmt.Fprintln(writer, "没有 Key。")
		return err
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "ID\tPRINCIPAL\tMACHINE\tSTATUS\tSOURCES\tEXPIRES"); err != nil {
		return err
	}
	for _, item := range items {
		if _, err := fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\t%s\n", item.CredentialID, safeHuman(item.PrincipalID), safeHuman(item.MachineID), safeHuman(item.Status), strings.Join(safeHumanSlice(item.AllowedSources), ","), formatOptionalTime(item.ExpiresAt)); err != nil {
			return err
		}
	}
	return table.Flush()
}

func writeCredential(writer io.Writer, item adminproto.Credential) error {
	_, err := fmt.Fprintf(writer, "ID：%d\nPrincipal：%s\n机器：%s\n状态：%s\n来源：%s\n到期：%s\n创建：%s\n更新：%s\n",
		item.CredentialID, safeHuman(item.PrincipalID), safeHuman(item.MachineID), safeHuman(item.Status), strings.Join(safeHumanSlice(item.AllowedSources), ", "),
		formatOptionalTime(item.ExpiresAt), formatTime(item.CreatedAt), formatTime(item.UpdatedAt))
	return err
}

func writeConnectionTable(writer io.Writer, items []adminproto.Connection) error {
	if len(items) == 0 {
		_, err := fmt.Fprintln(writer, "没有活动连接。")
		return err
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "CONNECTION\tPRINCIPAL\tMACHINE\tSOURCE\tREADY\tSESSIONS\tPAL"); err != nil {
		return err
	}
	for _, item := range items {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%t\t%d\t%s %s\n", safeHuman(item.ConnectionID), safeHuman(item.PrincipalID), safeHuman(item.MachineID), safeHuman(item.SourceIP), item.Ready, item.SessionCount, safeHuman(item.Implementation.Name), safeHuman(item.Implementation.Version)); err != nil {
			return err
		}
	}
	return table.Flush()
}

func writeConnection(writer io.Writer, item adminproto.Connection) error {
	_, err := fmt.Fprintf(writer, "Connection：%s\nCredential：%d\nPrincipal：%s\n机器：%s\n来源：%s\nPal：%s %s %s/%s\n连接时间：%s\n最近心跳：%s\n最近快照：%s（sequence %d，会话 %d）\nCapabilities：%s\nReady：%t\n",
		safeHuman(item.ConnectionID), item.CredentialID, safeHuman(item.PrincipalID), safeHuman(item.MachineID), safeHuman(item.SourceIP),
		safeHuman(item.Implementation.Name), safeHuman(item.Implementation.Version), safeHuman(item.Implementation.OS), safeHuman(item.Implementation.Arch),
		formatTime(item.ConnectedAt), formatTime(item.LastHeartbeatAt), formatTime(item.LastSnapshotAt), item.SnapshotSequence, item.SessionCount,
		strings.Join(safeHumanSlice(item.Capabilities), ", "), item.Ready)
	return err
}

func writeSessionTable(writer io.Writer, items []adminproto.Session) error {
	if len(items) == 0 {
		_, err := fmt.Fprintln(writer, "没有在线 Agent 会话。")
		return err
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "PRINCIPAL\t#\tMACHINE\tWORKSPACE\tAGENT\tPANE\tSTATUS"); err != nil {
		return err
	}
	for _, item := range items {
		agent := item.DisplayAgent
		if agent == "" {
			agent = item.Agent
		}
		if _, err := fmt.Fprintf(table, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n", safeHuman(item.PrincipalID), item.Number, safeHuman(item.Target.MachineID), safeHuman(item.WorkspaceLabel), safeHuman(agent), safeHuman(item.Pane), safeHuman(item.StatusLabel)); err != nil {
			return err
		}
	}
	return table.Flush()
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return formatTime(*value)
}

func formatTypeError(method adminproto.Method) error {
	return fmt.Errorf("%s 输出类型无效", method)
}

func safeHuman(value string) string {
	return strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return '\uFFFD'
		}
		return character
	}, value)
}

func safeHumanSlice(values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = safeHuman(value)
	}
	return result
}
