//go:build darwin || linux

package ffi

import "github.com/ebitengine/purego"

type dynamicLibrary struct {
	handle uintptr
}

func openDynamicLibrary(path string) (*dynamicLibrary, error) {
	handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, err
	}
	return &dynamicLibrary{handle: handle}, nil
}

func (library *dynamicLibrary) lookup(name string) (uintptr, error) {
	return purego.Dlsym(library.handle, name)
}

func (library *dynamicLibrary) close() error {
	if library == nil || library.handle == 0 {
		return nil
	}
	err := purego.Dlclose(library.handle)
	library.handle = 0
	return err
}
