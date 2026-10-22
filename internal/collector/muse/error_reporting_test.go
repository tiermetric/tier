package muse

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/logsafe"
)

type infoErrorEntry struct {
	fs.DirEntry
	err error
}

func (d infoErrorEntry) Info() (fs.FileInfo, error) { return nil, d.err }

func TestFindSessionFiles_ReportsInfoError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, sessionFileName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	infoErr := errors.New("injected file-info failure")
	c := &Collector{
		sessionsDir: root,
		logger:      quietLogger(),
		walkDir: func(root string, fn fs.WalkDirFunc) error {
			return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if p == path && err == nil {
					d = infoErrorEntry{d, infoErr}
				}
				return fn(p, d, err)
			})
		},
	}
	paths, err := c.findSessionFiles(time.Now().Add(time.Hour), nil)
	if !reflect.DeepEqual(paths, []string{path}) {
		t.Errorf("paths=%v, want unreadable-info file retained", paths)
	}
	if !errors.Is(err, infoErr) {
		t.Fatalf("findSessionFiles error=%v, want wrapped Info error", err)
	}
	if !strings.Contains(err.Error(), logsafe.Str(path)) {
		t.Errorf("findSessionFiles error=%v, missing file path", err)
	}
}
