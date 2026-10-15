//go:build !linux && !darwin

package reel

func discoverRecorderSocket(ProcessIdentity) (recorderSocket, error) {
	return recorderSocket{}, platformCheck()
}
func validateRecorderSocket(recorderSocket) error { return platformCheck() }
