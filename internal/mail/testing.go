//go:build !js

package mail

// SetTransport makes hubs created afterwards use t; the returned func restores the default. Tests only.
func SetTransport(t Transport) (restore func()) {
	prev := transportFactory
	transportFactory = func() Transport { return t }
	return func() { transportFactory = prev }
}
