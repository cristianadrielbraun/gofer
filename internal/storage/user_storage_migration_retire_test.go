package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRestoreSharedDatabaseRefusesStartedPublication(t *testing.T) {
	for _, state := range []string{"preparing", "verified", "publishing", "published"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "gofer.db")
			retired := path + ".shared"
			if err := os.WriteFile(retired, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			journal := migrationPreparationJournal{Version: 1, ID: "0123456789abcdef0123456789abcdef", State: state, SourcePath: retired, DestinationPath: path, Directory: path + ".staging"}
			if err := os.Mkdir(journal.Directory, 0700); err != nil {
				t.Fatal(err)
			}
			if err := writeMigrationPreparationJournal(path+".migration.json", journal); err != nil {
				t.Fatal(err)
			}
			err := RestoreSharedDatabase(path, retired)
			restorable := state == "preparing" || state == "verified"
			if (err == nil) != restorable {
				t.Fatal("restore decision", err)
			}
			restored, readErr := os.ReadFile(path)
			_, journalErr := os.Lstat(path + ".migration.json")
			_, stageErr := os.Lstat(journal.Directory)
			if restorable && (readErr != nil || string(restored) != "original" || !errors.Is(journalErr, os.ErrNotExist) || !errors.Is(stageErr, os.ErrNotExist)) {
				t.Fatal("unpublished migration not discarded", readErr, journalErr, stageErr)
			}
			if !restorable && (!errors.Is(readErr, os.ErrNotExist) || journalErr != nil || stageErr != nil) {
				t.Fatal("started publication changed", readErr, journalErr, stageErr)
			}
		})
	}
}
