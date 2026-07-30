package adminserver

import "github.com/wenxichang/herdr-pal/internal/adminservice"

// CredentialManager 是共享管理服务凭据依赖的兼容别名。
type CredentialManager = adminservice.CredentialManager

// ConnectionManager 是共享管理服务连接依赖的兼容别名。
type ConnectionManager = adminservice.ConnectionManager

// SessionInspector 是共享管理服务会话依赖的兼容别名。
type SessionInspector = adminservice.SessionInspector

// RuntimeInspector 是共享管理服务运行时依赖的兼容别名。
type RuntimeInspector = adminservice.RuntimeController
