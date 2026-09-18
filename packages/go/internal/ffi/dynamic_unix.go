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
	// easysql is built with Go's c-shared mode and owns process runtime state.
	// Unloading it and opening it again can deadlock in dyld on macOS, so keep
	// the loader reference pinned until process exit. Client.Close still marks
	// the client closed and prevents any subsequent calls through it.
	return nil
}
