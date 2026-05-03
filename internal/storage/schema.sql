-- gofer.email schema
-- Version 1: Initial schema

CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY,
    applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Accounts
CREATE TABLE IF NOT EXISTS accounts (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL DEFAULT 'imap',
    email_address TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    color TEXT NOT NULL DEFAULT '',
    initials TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Folders
CREATE TABLE IF NOT EXISTS folders (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    parent_id TEXT REFERENCES folders(id) ON DELETE CASCADE,
    remote_id TEXT,
    name TEXT NOT NULL,
    icon TEXT NOT NULL DEFAULT 'folder',
    role TEXT NOT NULL DEFAULT 'custom',
    sort_order INTEGER NOT NULL DEFAULT 0,
    uid_validity INTEGER,
    uid_next INTEGER,
    sync_cursor TEXT,
    total_count INTEGER NOT NULL DEFAULT 0,
    unread_count INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Messages
CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    remote_message_id TEXT,
    internet_message_id TEXT,
    thread_id TEXT,
    subject TEXT NOT NULL DEFAULT '',
    from_name TEXT NOT NULL DEFAULT '',
    from_email TEXT NOT NULL DEFAULT '',
    date_sent DATETIME,
    date_received DATETIME,
    snippet TEXT NOT NULL DEFAULT '',
    preview_text TEXT NOT NULL DEFAULT '',
    body_text_path TEXT,
    body_html_path TEXT,
    raw_path TEXT,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    has_attachments INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Message-to-folder mapping (a message can be in multiple folders/labels)
CREATE TABLE IF NOT EXISTS message_folder_state (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    folder_id TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    remote_uid INTEGER,
    is_read INTEGER NOT NULL DEFAULT 0,
    is_starred INTEGER NOT NULL DEFAULT 0,
    is_flagged INTEGER NOT NULL DEFAULT 0,
    is_draft INTEGER NOT NULL DEFAULT 0,
    is_deleted INTEGER NOT NULL DEFAULT 0,
    is_archived INTEGER NOT NULL DEFAULT 0,
    synced_at DATETIME,
    PRIMARY KEY (message_id, folder_id)
);

-- Recipients (normalized, not JSON)
CREATE TABLE IF NOT EXISTS message_recipients (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT ''
);

-- Attachments
CREATE TABLE IF NOT EXISTS attachments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    filename TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    content_id TEXT,
    inline INTEGER NOT NULL DEFAULT 0,
    storage_path TEXT NOT NULL,
    sha256 TEXT
);

-- Labels
CREATE TABLE IF NOT EXISTS labels (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    color TEXT NOT NULL DEFAULT ''
);

-- Message-label mapping
CREATE TABLE IF NOT EXISTS message_labels (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    label_id TEXT NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
    PRIMARY KEY (message_id, label_id)
);

-- Sync state per account+folder
CREATE TABLE IF NOT EXISTS sync_state (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    folder_id TEXT REFERENCES folders(id) ON DELETE CASCADE,
    cursor TEXT,
    last_success_at DATETIME,
    last_error TEXT,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Search documents (denormalized for FTS)
CREATE TABLE IF NOT EXISTS message_search_docs (
    message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL,
    subject TEXT NOT NULL DEFAULT '',
    sender TEXT NOT NULL DEFAULT '',
    recipients TEXT NOT NULL DEFAULT '',
    body_text TEXT NOT NULL DEFAULT '',
    attachment_names TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- FTS5 full-text search index (standalone, auto-populated via trigger)
CREATE VIRTUAL TABLE IF NOT EXISTS message_fts USING fts5(
    subject,
    sender,
    recipients,
    body
);

-- Trigger: auto-populate FTS when a message is inserted
CREATE TRIGGER IF NOT EXISTS trg_messages_after_insert
AFTER INSERT ON messages
BEGIN
    INSERT INTO message_fts(rowid, subject, sender, recipients, body)
    VALUES (
        NEW.id,
        NEW.subject,
        NEW.from_name || ' <' || NEW.from_email || '>',
        '',
        COALESCE(NEW.preview_text, '')
    );
END;

-- Indexes

CREATE INDEX IF NOT EXISTS idx_folders_account
ON folders(account_id, sort_order);

CREATE INDEX IF NOT EXISTS idx_folders_account_role
ON folders(account_id, role);

CREATE INDEX IF NOT EXISTS idx_messages_account_date
ON messages(account_id, date_received DESC);

CREATE INDEX IF NOT EXISTS idx_messages_thread
ON messages(account_id, thread_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_remote_id
ON messages(account_id, remote_message_id);

CREATE INDEX IF NOT EXISTS idx_messages_internet_msg_id
ON messages(internet_message_id);

CREATE INDEX IF NOT EXISTS idx_folder_state_folder_date
ON message_folder_state(folder_id, synced_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_folder_uid
ON message_folder_state(folder_id, remote_uid);

CREATE INDEX IF NOT EXISTS idx_recipients_email
ON message_recipients(email);

CREATE INDEX IF NOT EXISTS idx_recipients_message
ON message_recipients(message_id);

CREATE INDEX IF NOT EXISTS idx_attachments_message
ON attachments(message_id);

CREATE INDEX IF NOT EXISTS idx_sync_state_account
ON sync_state(account_id, folder_id);

CREATE INDEX IF NOT EXISTS idx_message_labels_message
ON message_labels(message_id);

CREATE INDEX IF NOT EXISTS idx_message_labels_label
ON message_labels(label_id);

CREATE INDEX IF NOT EXISTS idx_message_search_docs_account
ON message_search_docs(account_id);
