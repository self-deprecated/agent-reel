package visualstore

import "os"

func openFilePaths() ([]string, error) {
	fds, e := os.ReadDir("/proc/self/fd")
	if e != nil {
		return nil, e
	}
	var paths []string
	for _, fd := range fds {
		if p, e := os.Readlink("/proc/self/fd/" + fd.Name()); e == nil {
			paths = append(paths, p)
		}
	}
	return paths, nil
}
