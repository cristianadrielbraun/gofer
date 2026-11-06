package models

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// UnifiedTagID is stable across account order, provider, and display-name casing.
func UnifiedTagID(name string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(name))))
	return "unified_tag_" + hex.EncodeToString(sum[:])
}

func IsUnifiedTagID(id string) bool {
	if !strings.HasPrefix(id, "unified_tag_") || len(id) != len("unified_tag_")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "unified_tag_"))
	return err == nil
}

func TagColorKey(id string, unified bool) string {
	if unified {
		return "unified_tag_color:" + id
	}
	return "tag_color:" + id
}

// UnifiedTagOrderKey keeps a unified tag's place among the unified tags the
// user put in order.
func UnifiedTagOrderKey(id string) string { return "unified_tag_order:" + id }

func TagStyleKey(id string, unified bool) string {
	if unified {
		return "unified_tag_style:" + id
	}
	return "tag_style:" + id
}

// UnifiedTagAppearance keeps the combined tag independent of any account's copy.
func UnifiedTagAppearance(label Label, settings map[string]string) Label {
	label.ID = UnifiedTagID(label.Name)
	label.Name = strings.TrimSpace(label.Name)
	label.AccountID, label.ProviderID, label.ProviderType = "", "", ""
	label.IsUnified = true
	label.AppearanceColor = ""
	if color := settings[TagColorKey(label.ID, true)]; ValidFolderColor(color) {
		label.AppearanceColor = color
	}
	label.IconStyle = FolderIconStyle(settings[TagStyleKey(label.ID, true)])
	return label
}

// TagAppearanceFolder lets tags use the existing fixed-symbol appearance editor.
func TagAppearanceFolder(label Label) Folder {
	return Folder{ID: label.ID, Name: label.Name, Icon: "tag", Role: "tag", Color: label.AppearanceColor,
		IconStyle: label.IconStyle, IsSystem: true, IsTag: true, IsUnifiedTag: label.IsUnified}
}
