package app

import (
	"errors"
	"fmt"
)

type configError struct {
	detail string
}

func newConfigError(detail string) error {
	return &configError{detail: detail}
}

func (err *configError) Error() string {
	return fmt.Sprintf("%s: %s", ErrConfig, err.detail)
}

func (err *configError) Unwrap() error {
	return ErrConfig
}

// ConfigErrorDetail 返回应用内部确认可安全展示的配置错误详情。
func ConfigErrorDetail(err error) (string, bool) {
	var target *configError
	if !errors.As(err, &target) {
		return "", false
	}
	return target.detail, true
}
