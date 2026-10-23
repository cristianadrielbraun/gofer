package models

// MailboxSizeCount is how many messages take how much room on the mail
// server. Unmeasured messages have no size yet and count as 0 bytes.
// LocalBytes is what Gofer keeps of them on this device.
type MailboxSizeCount struct {
	Messages   int
	Bytes      int64
	Unmeasured int
	LocalBytes int64
}

// MailboxSize is an account's size on the mail server, in all and for each
// folder. A message kept in several folders, as Gmail labels do, counts once
// in Total and once in each of its folders. LocalOutsideFolders is kept on
// this device for messages in no folder, such as ones deleted but not yet
// cleaned up; Total.LocalBytes includes it.
type MailboxSize struct {
	Account             Account
	Total               MailboxSizeCount
	Folders             map[string]MailboxSizeCount
	HasLocal            bool
	LocalOutsideFolders int64
}
