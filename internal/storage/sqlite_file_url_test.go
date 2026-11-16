package storage

import "testing"

func TestSQLiteFileURLKeepsDriveInPath(t *testing.T) {
	for path, want := range map[string]string{
		"/srv/gofer/data/gofer.db":   "file:///srv/gofer/data/gofer.db",
		"C:/dev/gofer/data/gofer.db": "file:///C:/dev/gofer/data/gofer.db",
	} {
		uri := sqliteFileURL(path)
		if got := uri.String(); got != want {
			t.Errorf("sqliteFileURL(%q) = %q, want %q", path, got, want)
		}
	}
}
