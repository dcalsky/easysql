//go:build !darwin && !linux && !windows

package ffi

import "fmt"

type dynamicLibrary struct{}

func openDynamicLibrary(path string) (*dynamicLibrary, error) {
	return nil, fmt.Errorf("dynamic loading %q is unsupported on this platform", path)
}

func (library *dynamicLibrary) lookup(name string) (uintptr, error) {
	return 0, fmt.Errorf("symbol lookup %q is unsupported on this platform", name)
}

func (library *dynamicLibrary) close() error { return nil }
