//go:build !windows
// +build !windows

package daemon

func doClean() chan string {
	return nil
}
