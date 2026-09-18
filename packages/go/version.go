package easysql

import "runtime/debug"

const modulePath = "github.com/dcalsky/easysql/packages/go"

// Version returns the Go module version selected by the consuming program.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(devel)"
	}
	if info.Main.Path == modulePath {
		return buildVersion(info.Main)
	}
	for _, dependency := range info.Deps {
		if dependency.Path == modulePath {
			return buildVersion(*dependency)
		}
	}
	return "(devel)"
}

func buildVersion(module debug.Module) string {
	if module.Replace != nil {
		module = *module.Replace
	}
	if module.Version == "" {
		return "(devel)"
	}
	return module.Version
}
