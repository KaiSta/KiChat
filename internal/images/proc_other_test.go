//go:build !unix

package images

// processAlive has no equivalent where there is no signal 0 to send.
//
// Nothing reaches it: the only test that asks is the one whose stand-in upscaler is a
// shell script, and that test skips on these platforms before it starts a program.
func processAlive(int) bool { return false }
