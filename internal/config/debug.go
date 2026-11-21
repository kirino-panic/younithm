package config

import "os"

// EnvDebug turns on the Debug tab in Settings and the endpoints behind it
// (fake now playing, import size limit, clearing the library). Off unless set
// to 1 or true.
const EnvDebug = "YOUNITHM_DEBUG"

// DebugTools reports whether the debug tools are enabled.
func DebugTools() bool {
	v := os.Getenv(EnvDebug)
	return v == "1" || v == "true"
}
