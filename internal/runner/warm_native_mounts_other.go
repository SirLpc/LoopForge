//go:build !linux

package runner

import "errors"

var errNativeWarmOS = errors.New("native fixed warm workspaces require Linux")

func qualifyNativeWarmMounts(string) error   { return errNativeWarmOS }
func bindNativeWarmDir(string, string) error { return errNativeWarmOS }
func unmountNativeWarmDir(string) error      { return errNativeWarmOS }
