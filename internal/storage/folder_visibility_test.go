package storage

import (
	"errors"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func findVisibilityTestFolder(folders []models.Folder, id string) (models.Folder, bool) {
	for _, folder := range folders {
		if folder.ID == id {
			return folder, true
		}
		if found, ok := findVisibilityTestFolder(folder.Children, id); ok {
			return found, true
		}
	}
	return models.Folder{}, false
}

func TestFolderVisibilityPersistsAndSurvivesSync(t *testing.T) {
	db := seedManagedFolders(t)
	ctx := t.Context()

	get := func(id string) models.Folder {
		t.Helper()
		tree, err := db.AccountFolderTree(ctx, "acc")
		if err != nil {
			t.Fatal(err)
		}
		folder, ok := findVisibilityTestFolder(tree, id)
		if !ok {
			t.Fatalf("folder %s missing", id)
		}
		return folder
	}

	// Existing folders must be visible by default.
	if get("acc_dunes").Hidden || get("acc_trips").Hidden {
		t.Fatal("folders must be visible by default")
	}

	// Hide a nested folder.
	if err := db.SetFolderVisibility(ctx, "default", "acc_dunes", false); err != nil {
		t.Fatal(err)
	}
	if !get("acc_dunes").Hidden {
		t.Fatal("folder was not hidden")
	}

	// Provider synchronization must preserve visibility.
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{
			ID: "acc_dunes", AccountID: "acc",
			ParentID: "acc_coast",
			RemoteID: "Trips/Coast/Dunes",
			Name:     "Dunes Updated",
			Icon:     "folder", Role: "custom",
			Selectable: true,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !get("acc_dunes").Hidden {
		t.Fatal("sync reset visibility")
	}

	// Parent visibility must not overwrite child preferences.
	if err := db.SetFolderVisibility(ctx, "default", "acc_trips", false); err != nil {
		t.Fatal(err)
	}
	if !get("acc_trips").Hidden || !get("acc_dunes").Hidden {
		t.Fatal("nested visibility was not preserved")
	}

	if err := db.SetFolderVisibility(ctx, "default", "acc_trips", true); err != nil {
		t.Fatal(err)
	}
	if get("acc_trips").Hidden || !get("acc_dunes").Hidden {
		t.Fatal("showing parent changed child preference")
	}

	// Show the nested folder again.
	if err := db.SetFolderVisibility(ctx, "default", "acc_dunes", true); err != nil {
		t.Fatal(err)
	}
	if get("acc_dunes").Hidden {
		t.Fatal("folder remained hidden")
	}

	// Visible is the default: no stored override is needed.
	var count int
	err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM app_settings
		 WHERE user_id = ? AND key = ?`,
		"default", "folder_visible:acc_dunes",
	).Scan(&count)
	if err != nil || count != 0 {
		t.Fatalf("visible preference remains: count=%d err=%v", count, err)
	}
}

func TestFolderVisibilityRefusesUnauthorizedAndInactive(t *testing.T) {
	db := seedManagedFolders(t)
	ctx := t.Context()

	if err := db.SetFolderVisibility(
		ctx, "someone-else", "acc_dunes", false,
	); !errors.Is(err, ErrFolderNotManaged) {
		t.Fatalf("foreign write = %v", err)
	}

	if _, err := db.Write().ExecContext(ctx,
		`UPDATE folders SET discovery_state = 'missing'
		 WHERE id = 'acc_dunes'`,
	); err != nil {
		t.Fatal(err)
	}

	if err := db.SetFolderVisibility(
		ctx, "default", "acc_dunes", false,
	); !errors.Is(err, ErrFolderNotManaged) {
		t.Fatalf("inactive folder write = %v", err)
	}

	var count int
	err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM app_settings
		 WHERE key = 'folder_visible:acc_dunes'`,
	).Scan(&count)
	if err != nil || count != 0 {
		t.Fatalf("unauthorized preference written: count=%d err=%v", count, err)
	}
}

func TestFolderVisibilityRejectsSystemRoles(t *testing.T) {
	db := seedManagedFolders(t)
	ctx := t.Context()

	protectedRoles := []string{
		"inbox",
		"sent",
		"drafts",
		"trash",
		"junk",
		"spam",
		"archive",
		"outbox",
		"starred",
		"scheduled",
		"snoozed",
		"future-system-role",
		"",
	}

	for _, role := range protectedRoles {
		t.Run("role="+role, func(t *testing.T) {
			_, err := db.Write().ExecContext(ctx,
				`UPDATE folders SET role = ? WHERE id = 'acc_dunes'`,
				role,
			)
			if err != nil {
				t.Fatal(err)
			}

			// Neither Hide nor Show may modify a protected folder.
			for _, visible := range []bool{false, true} {
				err := db.SetFolderVisibility(
					ctx, "default", "acc_dunes", visible,
				)
				if !errors.Is(err, ErrFolderNotManaged) {
					t.Fatalf(
						"role=%q visible=%v: expected rejection, got %v",
						role, visible, err,
					)
				}
			}

			// Rejected operations must not create preferences.
			var count int
			err = db.Read().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM app_settings
                 WHERE user_id = 'default'
                   AND key = 'folder_visible:acc_dunes'`,
			).Scan(&count)

			if err != nil || count != 0 {
				t.Fatalf(
					"role=%q unexpected preference: count=%d err=%v",
					role, count, err,
				)
			}
		})
	}

	// Restore a normal folder role and verify it remains manageable.
	_, err := db.Write().ExecContext(ctx,
		`UPDATE folders SET role = 'custom'
         WHERE id = 'acc_dunes'`,
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.SetFolderVisibility(
		ctx, "default", "acc_dunes", false,
	); err != nil {
		t.Fatalf("custom folder cannot be hidden: %v", err)
	}

	if err := db.SetFolderVisibility(
		ctx, "default", "acc_dunes", true,
	); err != nil {
		t.Fatalf("custom folder cannot be shown: %v", err)
	}
}

func TestFolderVisibilitySystemRoleOverridesSavedPreference(t *testing.T) {
	db := seedManagedFolders(t)
	ctx := t.Context()

	get := func() models.Folder {
		t.Helper()
		tree, err := db.AccountFolderTree(ctx, "acc")
		if err != nil {
			t.Fatal(err)
		}
		folder, ok := findVisibilityTestFolder(tree, "acc_dunes")
		if !ok {
			t.Fatal("folder missing")
		}
		return folder
	}

	// Initially custom: hiding is allowed.
	if err := db.SetFolderVisibility(
		ctx, "default", "acc_dunes", false,
	); err != nil {
		t.Fatal(err)
	}
	if !get().Hidden {
		t.Fatal("custom folder should be hidden")
	}

	// IMAP reclassifies the folder as a system folder.
	_, err := db.Write().ExecContext(ctx,
		`UPDATE folders SET role = 'inbox'
         WHERE id = 'acc_dunes'`)
	if err != nil {
		t.Fatal(err)
	}

	// The old preference must not hide a system folder.
	if get().Hidden {
		t.Fatal("system folder hidden by old preference")
	}

	// If it becomes custom again, its preference is preserved.
	_, err = db.Write().ExecContext(ctx,
		`UPDATE folders SET role = 'custom'
         WHERE id = 'acc_dunes'`)
	if err != nil {
		t.Fatal(err)
	}

	if !get().Hidden {
		t.Fatal("original custom visibility preference was lost")
	}
}
