//go:build windows

package lifecycle

import "context"

type unsupportedControl struct{}

func newPlatformControlServer() StatusServer { return unsupportedControl{} }

func newPlatformControlClient() StatusClient { return unsupportedControl{} }

func (unsupportedControl) Run(context.Context, string, func() Status) error {
	return ErrUnsupported
}

func (unsupportedControl) Status(context.Context, string) (Status, error) {
	return Status{}, ErrUnsupported
}
