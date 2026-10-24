package media

import "os"

// fileStat is the verified view of one downloaded file.
type fileStat struct {
	size    int64
	regular bool
}

// statFile stats path and reports whether it is a regular file.
func statFile(path string) (fileStat, error) {
	st, err := os.Stat(path)
	if err != nil {
		return fileStat{}, err
	}
	return fileStat{size: st.Size(), regular: !st.IsDir() && st.Mode().IsRegular()}, nil
}
