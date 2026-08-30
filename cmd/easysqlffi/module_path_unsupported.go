//go:build !darwin && !linux && !windows

package main

import "errors"

func moduleDirectory() (string, error) {
	return "", errors.New("easysql: native library is unsupported on this operating system")
}
