package adminproto

// Method 是 HPAP/1 的稳定管理方法名。
type Method string

const (
	MethodServerStatus         Method = "server.status"
	MethodServerStop           Method = "server.stop"
	MethodServerDebugEnable    Method = "server.debug.enable"
	MethodServerDebugDisable   Method = "server.debug.disable"
	MethodKeyIssue             Method = "key.issue"
	MethodKeyList              Method = "key.list"
	MethodKeyShow              Method = "key.show"
	MethodKeyEnable            Method = "key.enable"
	MethodKeyDisable           Method = "key.disable"
	MethodKeyDelete            Method = "key.delete"
	MethodKeySourceList        Method = "key.source.list"
	MethodKeySourceAdd         Method = "key.source.add"
	MethodKeySourceRemove      Method = "key.source.remove"
	MethodKeySourceSet         Method = "key.source.set"
	MethodConnectionList       Method = "connection.list"
	MethodConnectionShow       Method = "connection.show"
	MethodConnectionDisconnect Method = "connection.disconnect"
	MethodSessionList          Method = "session.list"
)

var methods = [...]Method{
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

// Methods 返回 HPAP/1 支持的方法，调用方可以安全修改返回切片。
func Methods() []Method {
	return append([]Method(nil), methods[:]...)
}

// IsKnownMethod 报告方法是否属于 HPAP/1 固定方法集合。
func IsKnownMethod(method Method) bool {
	for _, current := range methods {
		if current == method {
			return true
		}
	}
	return false
}
