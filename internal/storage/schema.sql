-- Gofer schema
-- Version 1: Initial schema

CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY,
    applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Accounts
CREATE TABLE IF NOT EXISTS accounts (
    id TEXT PRIMARY KEY,
    user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
    provider TEXT NOT NULL DEFAULT 'imap',
    provider_account_id TEXT NOT NULL DEFAULT '',
    email_address TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    color TEXT NOT NULL DEFAULT '',
    initials TEXT NOT NULL DEFAULT '',
    imap_host TEXT NOT NULL DEFAULT '',
    imap_port INTEGER NOT NULL DEFAULT 993,
    imap_tls_mode TEXT NOT NULL DEFAULT 'tls',
    smtp_host TEXT NOT NULL DEFAULT '',
    smtp_port INTEGER NOT NULL DEFAULT 465,
    smtp_tls_mode TEXT NOT NULL DEFAULT 'tls',
    username TEXT NOT NULL DEFAULT '',
    encrypted_password BLOB,
    auth_method TEXT NOT NULL DEFAULT 'plain',
    smtp_username TEXT NOT NULL DEFAULT '',
    encrypted_smtp_password BLOB,
    is_deleting INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_accounts_user ON accounts(user_id);
CREATE INDEX IF NOT EXISTS idx_accounts_provider_identity ON accounts(provider, provider_account_id);

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
    highest_seen_uid INTEGER NOT NULL DEFAULT 0,
    highest_modseq INTEGER,
    last_full_sync_at DATETIME,
    last_incremental_sync_at DATETIME,
    sync_error TEXT,
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
    message_id_normalized TEXT NOT NULL DEFAULT '',
    thread_id TEXT,
    thread_parent_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    provider_thread_id TEXT,
    in_reply_to TEXT NOT NULL DEFAULT '',
    "references" TEXT NOT NULL DEFAULT '',
    normalized_subject TEXT NOT NULL DEFAULT '',
    subject TEXT NOT NULL DEFAULT '',
    from_name TEXT NOT NULL DEFAULT '',
    from_email TEXT NOT NULL DEFAULT '',
    date_sent DATETIME,
    date_received DATETIME,
    snippet TEXT NOT NULL DEFAULT '',
    preview_text TEXT NOT NULL DEFAULT '',
    body_text_path TEXT,
    body_html_path TEXT,
    body_html_original_path TEXT,
    raw_path TEXT,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    has_attachments INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Conversation threads
CREATE TABLE IF NOT EXISTS threads (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    subject TEXT NOT NULL DEFAULT '',
    normalized_subject TEXT NOT NULL DEFAULT '',
    root_message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    last_message_at DATETIME,
    message_count INTEGER NOT NULL DEFAULT 0,
    unread_count INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Ordered RFC References chain for each message
CREATE TABLE IF NOT EXISTS message_references (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    referenced_message_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    PRIMARY KEY (message_id, ordinal)
);

-- References whose parent message has not arrived yet
CREATE TABLE IF NOT EXISTS unresolved_references (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    referenced_message_id TEXT NOT NULL,
    child_message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, referenced_message_id, child_message_id, ordinal)
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

CREATE TABLE IF NOT EXISTS folder_thread_state (
    folder_id TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    thread_key TEXT NOT NULL,
    head_message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    last_message_at DATETIME,
    thread_count INTEGER NOT NULL DEFAULT 1,
    thread_is_read INTEGER NOT NULL DEFAULT 1,
    thread_is_starred INTEGER NOT NULL DEFAULT 0,
    thread_has_attachments INTEGER NOT NULL DEFAULT 0,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (folder_id, thread_key)
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

CREATE INDEX IF NOT EXISTS idx_folders_role_account
ON folders(role, account_id, id);

CREATE INDEX IF NOT EXISTS idx_messages_account_date
ON messages(account_id, date_received DESC);

CREATE INDEX IF NOT EXISTS idx_messages_account_date_id
ON messages(account_id, date_received DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_messages_thread
ON messages(account_id, thread_id);

CREATE INDEX IF NOT EXISTS idx_messages_thread_date
ON messages(account_id, thread_id, date_received);

CREATE INDEX IF NOT EXISTS idx_messages_msgid_norm
ON messages(account_id, message_id_normalized);

CREATE INDEX IF NOT EXISTS idx_threads_account_last
ON threads(account_id, last_message_at DESC);

CREATE INDEX IF NOT EXISTS idx_threads_subject
ON threads(account_id, normalized_subject, last_message_at DESC);

CREATE INDEX IF NOT EXISTS idx_message_references_ref
ON message_references(referenced_message_id);

CREATE INDEX IF NOT EXISTS idx_unresolved_references_ref
ON unresolved_references(account_id, referenced_message_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_remote_id
ON messages(account_id, remote_message_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_account_internet_msg_id
ON messages(account_id, internet_message_id);

CREATE INDEX IF NOT EXISTS idx_folder_state_folder_date
ON message_folder_state(folder_id, synced_at DESC);

CREATE INDEX IF NOT EXISTS idx_folder_state_folder_deleted_msg
ON message_folder_state(folder_id, is_deleted, message_id);

CREATE INDEX IF NOT EXISTS idx_folder_state_starred_deleted_msg
ON message_folder_state(is_starred, is_deleted, message_id);

CREATE INDEX IF NOT EXISTS idx_folder_thread_state_folder_last
ON folder_thread_state(folder_id, last_message_at DESC, head_message_id DESC);

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

-- Application settings
CREATE TABLE IF NOT EXISTS app_settings (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, key)
);

CREATE TABLE IF NOT EXISTS remote_content_senders (
    sender_email TEXT PRIMARY KEY,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS remote_content_messages (
    message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE
);

-- Users (application-level authentication)
CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL DEFAULT '',
    avatar_url TEXT NOT NULL DEFAULT '',
    is_admin INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- OAuth provider accounts (Google, future: GitHub, etc.)
CREATE TABLE IF NOT EXISTS oauth_accounts (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    provider_account_id TEXT NOT NULL,
    access_token TEXT NOT NULL DEFAULT '',
    refresh_token TEXT NOT NULL DEFAULT '',
    token_type TEXT NOT NULL DEFAULT 'Bearer',
    expires_at DATETIME,
    scopes TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_oauth_provider_account
    ON oauth_accounts(provider, provider_account_id);

CREATE INDEX IF NOT EXISTS idx_oauth_accounts_user
    ON oauth_accounts(user_id);

-- Sessions
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token TEXT NOT NULL UNIQUE,
    user_agent TEXT NOT NULL DEFAULT '',
    expires_at DATETIME NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_sessions_user
    ON sessions(user_id);

CREATE INDEX IF NOT EXISTS idx_sessions_token
    ON sessions(token);

CREATE INDEX IF NOT EXISTS idx_sessions_expires
    ON sessions(expires_at);

-- Shared compose signatures
CREATE TABLE IF NOT EXISTS signatures (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    html_body TEXT NOT NULL DEFAULT '',
    text_body TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_signatures_user
    ON signatures(user_id, name);

CREATE TABLE IF NOT EXISTS account_signature_settings (
    account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    new_signature_id TEXT REFERENCES signatures(id) ON DELETE SET NULL,
    reply_signature_id TEXT REFERENCES signatures(id) ON DELETE SET NULL,
    forward_signature_id TEXT REFERENCES signatures(id) ON DELETE SET NULL,
    new_enabled INTEGER NOT NULL DEFAULT 0,
    reply_enabled INTEGER NOT NULL DEFAULT 0,
    forward_enabled INTEGER NOT NULL DEFAULT 0,
    reply_placement TEXT NOT NULL DEFAULT 'before',
    forward_placement TEXT NOT NULL DEFAULT 'before',
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS sender_avatars (
    email_hash TEXT PRIMARY KEY,
    email TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'gravatar',
    gravatar_status TEXT NOT NULL DEFAULT 'unchecked',
    gravatar_checked_at DATETIME,
    bimi_status TEXT NOT NULL DEFAULT 'unchecked',
    bimi_checked_at DATETIME,
    status TEXT NOT NULL DEFAULT 'pending',
    content_type TEXT NOT NULL DEFAULT '',
    image_data BLOB,
    storage_path TEXT NOT NULL DEFAULT '',
    fetched_at DATETIME,
    expires_at DATETIME,
    next_retry_at DATETIME,
    error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_sender_avatars_status_retry
ON sender_avatars(status, next_retry_at);

CREATE INDEX IF NOT EXISTS idx_sender_avatars_expires
ON sender_avatars(expires_at);

CREATE TABLE IF NOT EXISTS avatar_attempt_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email_hash TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL,
    status TEXT NOT NULL,
    message TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_avatar_attempt_logs_created
ON avatar_attempt_logs(created_at DESC);

CREATE INDEX IF NOT EXISTS idx_avatar_attempt_logs_provider_status
ON avatar_attempt_logs(provider, status, created_at DESC);

CREATE TABLE IF NOT EXISTS avatar_provider_states (
    email_hash TEXT NOT NULL,
    email TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'unchecked',
    message TEXT NOT NULL DEFAULT '',
    checked_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (email_hash, provider)
);

CREATE INDEX IF NOT EXISTS idx_avatar_provider_states_provider_status
ON avatar_provider_states(provider, status);

CREATE TABLE IF NOT EXISTS contacts (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    display_name TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'observed',
    is_manual INTEGER NOT NULL DEFAULT 0,
    is_deleted INTEGER NOT NULL DEFAULT 0,
    suppress_auto_create INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS contact_emails (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    email TEXT NOT NULL,
    normalized_email TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    is_primary INTEGER NOT NULL DEFAULT 0,
    observed_name TEXT NOT NULL DEFAULT '',
    message_count INTEGER NOT NULL DEFAULT 0,
    last_seen_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, normalized_email),
    UNIQUE(contact_id, normalized_email)
);

CREATE TABLE IF NOT EXISTS contact_sources (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    account_id TEXT NOT NULL DEFAULT '',
    remote_id TEXT NOT NULL DEFAULT '',
    etag TEXT NOT NULL DEFAULT '',
    sync_token TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_contacts_user_name
ON contacts(user_id, is_deleted, display_name COLLATE NOCASE);

CREATE INDEX IF NOT EXISTS idx_contact_emails_contact
ON contact_emails(contact_id);

CREATE INDEX IF NOT EXISTS idx_contact_emails_search
ON contact_emails(user_id, normalized_email);

CREATE INDEX IF NOT EXISTS idx_contact_sources_contact
ON contact_sources(contact_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_contact_sources_contact_provider_account
ON contact_sources(user_id, contact_id, provider, account_id);

CREATE INDEX IF NOT EXISTS idx_contact_sources_remote
ON contact_sources(user_id, provider, account_id, remote_id);

CREATE TABLE IF NOT EXISTS contact_save_targets (
    contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (contact_id, target)
);

CREATE INDEX IF NOT EXISTS idx_contact_save_targets_user
ON contact_save_targets(user_id, target);

CREATE TABLE IF NOT EXISTS contact_activity_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    email TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    event_count INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_contact_activity_events_user_created
ON contact_activity_events(user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_contact_activity_events_type_created
ON contact_activity_events(event_type, created_at DESC);

-- Schema version marker for fresh installs
INSERT OR REPLACE INTO schema_version (version) VALUES (29);
