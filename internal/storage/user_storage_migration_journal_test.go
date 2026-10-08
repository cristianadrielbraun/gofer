package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUserStorageMigrationJournalRetryRequiresExactSourceAndPreservesPreviousAttempt(t *testing.T) {
	for _, failure := range []string{"", "changed-database", "changed-wal", "changed-files", "changed-working-directory", "corrupt-journal", "foreign-binding", "symlink-stage", "before-mkdir", "before-binding"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			source, destination := filepath.Join(root, "source.db"), filepath.Join(root, "owned.db")
			if err := os.WriteFile(source, []byte("original database"), 0600); err != nil {
				t.Fatal(err)
			}
			files := UserStorageMigrationFiles{Version: 1, TreeDigest: strings.Repeat("a", 64), ReferenceDigest: strings.Repeat("b", 64)}
			journal, err := migrationAttemptJournal(t.Context(), source, destination, root, files, false)
			if err != nil {
				t.Fatal(err)
			}
			if failure != "before-mkdir" {
				if err := os.Mkdir(journal.Directory, 0700); err != nil {
					t.Fatal(err)
				}
				if failure != "before-binding" {
					if err := writeMigrationPreparationJournal(filepath.Join(journal.Directory, "PREPARATION.json"), journal); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(journal.Directory, "retain.txt"), []byte("failed attempt retained"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			workingDirectory := root
			switch failure {
			case "changed-database":
				err = os.WriteFile(source, []byte("changed source"), 0600)
			case "changed-wal":
				err = os.WriteFile(source+"-wal", []byte("new WAL"), 0600)
			case "changed-files":
				files.TreeDigest = strings.Repeat("c", 64)
			case "changed-working-directory":
				workingDirectory = t.TempDir()
			case "corrupt-journal":
				err = os.WriteFile(destination+".migration.json", []byte("invalid"), 0600)
			case "foreign-binding":
				binding := journal
				binding.WorkingDirectory = t.TempDir()
				err = writeMigrationPreparationJournal(filepath.Join(journal.Directory, "PREPARATION.json"), binding)
			case "symlink-stage":
				err = os.Rename(journal.Directory, journal.Directory+".retained")
				if err == nil {
					err = os.Symlink(journal.Directory+".retained", journal.Directory)
				}
				if err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			next, err := migrationAttemptJournal(t.Context(), source, destination, workingDirectory, files, true)
			valid := failure == "" || failure == "before-mkdir" || failure == "before-binding"
			if (err == nil) != valid {
				t.Fatal("retry decision", err)
			}
			if valid && (next.ID == journal.ID || next.Directory == journal.Directory || next.State != "preparing") {
				t.Fatal("old partial stage reused", next)
			}
			if failure != "before-mkdir" && failure != "symlink-stage" {
				retained, err := os.ReadFile(filepath.Join(journal.Directory, "retain.txt"))
				if err != nil || string(retained) != "failed attempt retained" {
					t.Fatal("prior attempt changed", err)
				}
			}
		})
	}
}

func TestUserStorageMigrationJournalReaderRefusesUnboundedUnrecognizedAndSymlinkFiles(t *testing.T) {
	for _, contents := range []string{"{} trailing", `{"unknown":true}`, strings.Repeat("x", 64*1024+1)} {
		path := filepath.Join(t.TempDir(), "journal.json")
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readMigrationPreparationJournal(path); err == nil {
			t.Fatal("invalid journal accepted")
		}
	}
	root := t.TempDir()
	pathToJournal := filepath.Join(root, "journal.json")
	if err := os.WriteFile(pathToJournal, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(pathToJournal, link); err == nil {
		if _, err := readMigrationPreparationJournal(link); err == nil {
			t.Fatal("symlink journal accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	path := filepath.Join(t.TempDir(), "source.db")
	if err := os.WriteFile(path, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := migrationSnapshotSource(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal("source hashing ignored cancellation", err)
	}
	if err := os.WriteFile(path+"-wal", []byte("original wal"), 0600); err != nil {
		t.Fatal(err)
	}
	proof, err := migrationSnapshotSource(t.Context(), path)
	if err != nil || !proof.WALPresent || proof.WALBytes != 12 || len(proof.WALDigest) != 64 {
		t.Fatal("WAL snapshot incomplete", proof, err)
	}
}
