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
    email_sync_enabled INTEGER NOT NULL DEFAULT 1,
    email_sync_error TEXT NOT NULL DEFAULT '',
    email_sync_error_at DATETIME,
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
    provider_remote_id TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,
    icon TEXT NOT NULL DEFAULT 'folder',
    role TEXT NOT NULL DEFAULT 'custom',
    selectable INTEGER NOT NULL DEFAULT 1,
    sort_order INTEGER NOT NULL DEFAULT 0,
    uid_validity INTEGER,
    uid_next INTEGER,
    sync_cursor TEXT,
    sync_progress_current INTEGER NOT NULL DEFAULT 0,
    sync_progress_started_at DATETIME,
    highest_seen_uid INTEGER NOT NULL DEFAULT 0,
    highest_modseq INTEGER,
    last_full_sync_at DATETIME,
    last_incremental_sync_at DATETIME,
    sync_error TEXT,
    total_count INTEGER NOT NULL DEFAULT 0,
    unread_count INTEGER NOT NULL DEFAULT 0,
    provider_count_drift_first_seen_at DATETIME,
    provider_count_drift_last_seen_at DATETIME,
    provider_count_drift_local_count INTEGER NOT NULL DEFAULT 0,
    provider_count_drift_remote_count INTEGER NOT NULL DEFAULT 0,
    provider_count_drift_cursor TEXT NOT NULL DEFAULT '',
    provider_count_drift_confirmations INTEGER NOT NULL DEFAULT 0,
    last_seen_at DATETIME,
    missing_since DATETIME,
    discovery_state TEXT NOT NULL DEFAULT 'active',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS folder_id_aliases (
    old_id TEXT PRIMARY KEY,
    new_id TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_folders_account_provider_remote
ON folders(account_id, provider_remote_id)
WHERE provider_remote_id != '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_folders_account_remote
ON folders(account_id, remote_id)
WHERE provider_remote_id = '' AND COALESCE(remote_id, '') != '';

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
    sha256 TEXT,
    provider_remote_id TEXT NOT NULL DEFAULT ''
);

-- Labels
CREATE TABLE IF NOT EXISTS labels (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    color TEXT NOT NULL DEFAULT '',
    provider_id TEXT NOT NULL DEFAULT '',
    provider_type TEXT NOT NULL DEFAULT '',
    is_system INTEGER NOT NULL DEFAULT 0,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Message-label mapping
CREATE TABLE IF NOT EXISTS message_labels (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    label_id TEXT NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
    PRIMARY KEY (message_id, label_id)
);

CREATE TABLE IF NOT EXISTS label_sync_state (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    provider_type TEXT NOT NULL,
    scope TEXT NOT NULL DEFAULT '',
    cursor TEXT NOT NULL DEFAULT '',
    last_full_sync_at DATETIME,
    last_success_at DATETIME,
    last_error TEXT NOT NULL DEFAULT '',
    last_run_started_at DATETIME,
    last_run_finished_at DATETIME,
    last_total_messages INTEGER NOT NULL DEFAULT 0,
    last_synced_messages INTEGER NOT NULL DEFAULT 0,
    last_with_labels INTEGER NOT NULL DEFAULT 0,
    last_without_labels INTEGER NOT NULL DEFAULT 0,
    last_missing_provider_messages INTEGER NOT NULL DEFAULT 0,
    last_skipped_messages INTEGER NOT NULL DEFAULT 0,
    last_failed_messages INTEGER NOT NULL DEFAULT 0,
    last_pending_mutations INTEGER NOT NULL DEFAULT 0,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, provider_type, scope)
);

CREATE TABLE IF NOT EXISTS label_mutation_queue (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    folder_id TEXT NOT NULL DEFAULT '',
    provider_type TEXT NOT NULL,
    operation TEXT NOT NULL,
    label_name TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS message_mutations (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    folder_id TEXT NOT NULL DEFAULT '',
    provider_type TEXT NOT NULL CHECK (provider_type IN ('gmail', 'outlook', 'imap')),
    kind TEXT NOT NULL CHECK (kind IN ('read', 'starred', 'move', 'delete')),
    target_value INTEGER NOT NULL CHECK (target_value IN (0, 1)),
    destination_folder_id TEXT NOT NULL DEFAULT '',
    source_uid_validity INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'failed', 'applied')),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    locked_at DATETIME,
    next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(message_id, kind, folder_id)
);

CREATE TABLE IF NOT EXISTS label_aliases (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    provider_type TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    display_name TEXT NOT NULL,
    color TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, provider_type, provider_id)
);

CREATE TABLE IF NOT EXISTS gmail_poll_state (
    account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    profile_history_id TEXT NOT NULL DEFAULT '',
    last_checked_at DATETIME,
    last_changed_at DATETIME,
    last_error TEXT NOT NULL DEFAULT '',
    consecutive_errors INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
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

-- Canonical maintained search index used by the mail list filters.
CREATE VIRTUAL TABLE IF NOT EXISTS message_search USING fts5(
    account_id UNINDEXED,
    thread_key UNINDEXED,
    subject,
    sender,
    recipients,
    snippet,
    body,
    attachment_names,
    tokenize='unicode61 remove_diacritics 2'
);

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

CREATE INDEX IF NOT EXISTS idx_messages_thread_parent
ON messages(thread_parent_id);

CREATE INDEX IF NOT EXISTS idx_threads_account_last
ON threads(account_id, last_message_at DESC);

CREATE INDEX IF NOT EXISTS idx_threads_subject
ON threads(account_id, normalized_subject, last_message_at DESC);

CREATE INDEX IF NOT EXISTS idx_threads_root_message
ON threads(root_message_id);

CREATE INDEX IF NOT EXISTS idx_message_references_ref
ON message_references(referenced_message_id);

CREATE INDEX IF NOT EXISTS idx_unresolved_references_ref
ON unresolved_references(account_id, referenced_message_id);

CREATE INDEX IF NOT EXISTS idx_unresolved_references_child
ON unresolved_references(child_message_id);

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

CREATE INDEX IF NOT EXISTS idx_folder_thread_state_account
ON folder_thread_state(account_id);

CREATE INDEX IF NOT EXISTS idx_folder_thread_state_head
ON folder_thread_state(head_message_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_folder_uid
ON message_folder_state(folder_id, remote_uid);

CREATE INDEX IF NOT EXISTS idx_recipients_email
ON message_recipients(email);

CREATE INDEX IF NOT EXISTS idx_recipients_message
ON message_recipients(message_id);

CREATE INDEX IF NOT EXISTS idx_attachments_message
ON attachments(message_id);
CREATE INDEX IF NOT EXISTS idx_attachments_message_provider_remote
ON attachments(message_id, provider_remote_id);

CREATE INDEX IF NOT EXISTS idx_sync_state_account
ON sync_state(account_id, folder_id);

CREATE INDEX IF NOT EXISTS idx_message_labels_message
ON message_labels(message_id);

CREATE INDEX IF NOT EXISTS idx_message_labels_label
ON message_labels(label_id);

CREATE INDEX IF NOT EXISTS idx_labels_account_name
ON labels(account_id, name COLLATE NOCASE);

CREATE INDEX IF NOT EXISTS idx_labels_account_provider
ON labels(account_id, provider_type, provider_id);

CREATE INDEX IF NOT EXISTS idx_label_sync_state_account
ON label_sync_state(account_id, provider_type);

CREATE INDEX IF NOT EXISTS idx_label_mutation_queue_due
ON label_mutation_queue(account_id, provider_type, next_attempt_at);

CREATE UNIQUE INDEX IF NOT EXISTS idx_label_mutation_queue_unique
ON label_mutation_queue(message_id, provider_type, operation, label_name COLLATE NOCASE);

CREATE INDEX IF NOT EXISTS idx_message_mutations_due
ON message_mutations(status, next_attempt_at, created_at);

CREATE INDEX IF NOT EXISTS idx_message_mutations_account
ON message_mutations(account_id, status, next_attempt_at);

CREATE INDEX IF NOT EXISTS idx_label_aliases_display
ON label_aliases(account_id, provider_type, display_name COLLATE NOCASE);

CREATE INDEX IF NOT EXISTS idx_gmail_poll_state_checked
ON gmail_poll_state(last_checked_at);

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

CREATE TABLE IF NOT EXISTS oauth_account_flows (
    state_hash TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_token_hash TEXT NOT NULL,
    provider TEXT NOT NULL,
    form_data TEXT NOT NULL,
    expires_at DATETIME NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_oauth_account_flows_expires
    ON oauth_account_flows(expires_at);

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
    address_book_id TEXT NOT NULL DEFAULT '',
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

CREATE UNIQUE INDEX IF NOT EXISTS idx_contact_sources_contact_provider_account_remote
ON contact_sources(user_id, contact_id, provider, account_id, remote_id);

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

CREATE TABLE IF NOT EXISTS account_contact_sync_configs (
    account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider TEXT NOT NULL DEFAULT 'carddav',
    enabled INTEGER NOT NULL DEFAULT 0,
    base_url TEXT NOT NULL DEFAULT '',
    addressbook_url TEXT NOT NULL DEFAULT '',
    username TEXT NOT NULL DEFAULT '',
    encrypted_password BLOB,
    last_sync_token TEXT NOT NULL DEFAULT '',
    last_started_at DATETIME,
    last_success_at DATETIME,
    last_import_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_account_contact_sync_configs_user
ON account_contact_sync_configs(user_id, enabled, provider);

CREATE TABLE IF NOT EXISTS account_contact_address_books (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    id TEXT NOT NULL,
    url TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    is_default INTEGER NOT NULL DEFAULT 0,
    last_sync_token TEXT NOT NULL DEFAULT '',
    last_success_at DATETIME,
    last_error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, url)
);

CREATE INDEX IF NOT EXISTS idx_account_contact_address_books_user
ON account_contact_address_books(user_id, account_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_account_contact_address_books_id
ON account_contact_address_books(id);

CREATE TABLE IF NOT EXISTS contact_sync_operations (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    contact_id TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT '',
    payload_json TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',
    attempt_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    locked_at DATETIME,
    next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_contact_sync_operations_due
ON contact_sync_operations(status, next_attempt_at, locked_at);

CREATE INDEX IF NOT EXISTS idx_contact_sync_operations_contact
ON contact_sync_operations(user_id, contact_id, created_at DESC);

CREATE TABLE IF NOT EXISTS contact_profiles (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    display_name TEXT NOT NULL DEFAULT '',
    sort_name TEXT NOT NULL DEFAULT '',
    primary_email TEXT NOT NULL DEFAULT '',
    avatar_url TEXT NOT NULL DEFAULT '',
    notes TEXT NOT NULL DEFAULT '',
    is_deleted INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_contact_profiles_user_name
ON contact_profiles(user_id, is_deleted, sort_name COLLATE NOCASE, display_name COLLATE NOCASE);

CREATE INDEX IF NOT EXISTS idx_contact_profiles_user_updated
ON contact_profiles(user_id, is_deleted, updated_at DESC, display_name COLLATE NOCASE);

CREATE TABLE IF NOT EXISTS contact_cards (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    profile_id TEXT NOT NULL REFERENCES contact_profiles(id) ON DELETE CASCADE,
    kind TEXT NOT NULL DEFAULT 'local',
    provider TEXT NOT NULL DEFAULT '',
    account_id TEXT NOT NULL DEFAULT '',
    address_book_id TEXT NOT NULL DEFAULT '',
    remote_id TEXT NOT NULL DEFAULT '',
    etag TEXT NOT NULL DEFAULT '',
    raw_payload TEXT NOT NULL DEFAULT '',
    raw_payload_type TEXT NOT NULL DEFAULT '',
    sync_status TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    is_deleted INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_contact_cards_profile
ON contact_cards(user_id, profile_id, is_deleted);

CREATE INDEX IF NOT EXISTS idx_contact_cards_remote
ON contact_cards(user_id, provider, account_id, address_book_id, remote_id);

CREATE INDEX IF NOT EXISTS idx_contact_cards_account
ON contact_cards(account_id);

CREATE TABLE IF NOT EXISTS contact_sync_memberships (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    profile_id TEXT NOT NULL REFERENCES contact_profiles(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL DEFAULT '',
    address_book_id TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL DEFAULT 'active',
    last_error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, profile_id, account_id, address_book_id)
);

CREATE INDEX IF NOT EXISTS idx_contact_sync_memberships_profile
ON contact_sync_memberships(user_id, profile_id, enabled);

CREATE INDEX IF NOT EXISTS idx_contact_sync_memberships_account
ON contact_sync_memberships(account_id, enabled);

CREATE TABLE IF NOT EXISTS contact_fields (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    profile_id TEXT NOT NULL REFERENCES contact_profiles(id) ON DELETE CASCADE,
    card_id TEXT REFERENCES contact_cards(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    value TEXT NOT NULL DEFAULT '',
    normalized_value TEXT NOT NULL DEFAULT '',
    is_primary INTEGER NOT NULL DEFAULT 0,
    ordinal INTEGER NOT NULL DEFAULT 0,
    source TEXT NOT NULL DEFAULT '',
    confidence REAL NOT NULL DEFAULT 1.0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_contact_fields_profile
ON contact_fields(user_id, profile_id, kind, ordinal);

CREATE INDEX IF NOT EXISTS idx_contact_fields_lookup
ON contact_fields(user_id, kind, normalized_value);

CREATE TABLE IF NOT EXISTS contact_field_preferences (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    profile_id TEXT NOT NULL REFERENCES contact_profiles(id) ON DELETE CASCADE,
    field_kind TEXT NOT NULL,
    preferred_normalized_value TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, profile_id, field_kind)
);

CREATE TABLE IF NOT EXISTS contact_identities (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    profile_id TEXT NOT NULL REFERENCES contact_profiles(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    normalized_value TEXT NOT NULL,
    confidence REAL NOT NULL DEFAULT 1.0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, kind, normalized_value)
);

CREATE INDEX IF NOT EXISTS idx_contact_identities_profile
ON contact_identities(user_id, profile_id);

CREATE TABLE IF NOT EXISTS contact_observations (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    profile_id TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT '',
    normalized_email TEXT NOT NULL,
    observed_name TEXT NOT NULL DEFAULT '',
    message_count INTEGER NOT NULL DEFAULT 0,
    last_seen_at DATETIME,
    is_suppressed INTEGER NOT NULL DEFAULT 0,
    suppress_auto_create INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, normalized_email)
);

CREATE INDEX IF NOT EXISTS idx_contact_observations_profile
ON contact_observations(user_id, profile_id);

CREATE INDEX IF NOT EXISTS idx_contact_observations_profile_active
ON contact_observations(user_id, profile_id, is_suppressed, last_seen_at, message_count);

CREATE INDEX IF NOT EXISTS idx_contact_observations_suppressed
ON contact_observations(user_id, is_suppressed, updated_at DESC);

CREATE TABLE IF NOT EXISTS contact_groups (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider TEXT NOT NULL DEFAULT '',
    account_id TEXT NOT NULL DEFAULT '',
    remote_id TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL DEFAULT '',
    color TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_contact_groups_user_name
ON contact_groups(user_id, name COLLATE NOCASE);

CREATE UNIQUE INDEX IF NOT EXISTS idx_contact_groups_remote
ON contact_groups(user_id, provider, account_id, remote_id);

CREATE INDEX IF NOT EXISTS idx_contact_groups_account
ON contact_groups(account_id);

CREATE TABLE IF NOT EXISTS contact_card_groups (
    card_id TEXT NOT NULL REFERENCES contact_cards(id) ON DELETE CASCADE,
    group_id TEXT NOT NULL REFERENCES contact_groups(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (card_id, group_id)
);

CREATE INDEX IF NOT EXISTS idx_contact_card_groups_user
ON contact_card_groups(user_id, group_id);

CREATE TABLE IF NOT EXISTS contact_conflicts (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    profile_id TEXT NOT NULL REFERENCES contact_profiles(id) ON DELETE CASCADE,
    field_kind TEXT NOT NULL DEFAULT '',
    local_value TEXT NOT NULL DEFAULT '',
    remote_value TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL DEFAULT '',
    account_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'open',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_contact_conflicts_profile
ON contact_conflicts(user_id, profile_id, status);

CREATE INDEX IF NOT EXISTS idx_contact_conflicts_account
ON contact_conflicts(account_id);

CREATE TABLE IF NOT EXISTS web_push_subscriptions (
    endpoint TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    p256dh TEXT NOT NULL,
    auth TEXT NOT NULL,
    user_agent TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_web_push_subscriptions_user
ON web_push_subscriptions(user_id);

CREATE TABLE IF NOT EXISTS outgoing_sends (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    draft_id TEXT NOT NULL DEFAULT '',
    transport TEXT NOT NULL CHECK (transport IN ('smtp', 'gmail', 'outlook')),
    envelope_from TEXT NOT NULL,
    envelope_recipients TEXT NOT NULL DEFAULT '[]',
    mime_data BLOB,
    message_json TEXT NOT NULL DEFAULT '',
    send_after DATETIME NOT NULL,
    next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    is_scheduled INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending',
    attempt_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    locked_at DATETIME,
    sent_message_id TEXT NOT NULL DEFAULT '',
    sent_copy_status TEXT NOT NULL DEFAULT 'not_required' CHECK (sent_copy_status IN ('not_required', 'pending', 'copying', 'complete', 'failed', 'ambiguous')),
    sent_copy_attempt_count INTEGER NOT NULL DEFAULT 0,
    sent_copy_last_error TEXT NOT NULL DEFAULT '',
    sent_copy_locked_at DATETIME,
    sent_copy_next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    sent_copy_uid INTEGER NOT NULL DEFAULT 0,
    sent_copy_uid_validity INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(message_id)
);

CREATE INDEX IF NOT EXISTS idx_outgoing_sends_due
ON outgoing_sends(status, next_attempt_at, send_after);

CREATE INDEX IF NOT EXISTS idx_outgoing_sends_account
ON outgoing_sends(account_id, status, send_after);

CREATE INDEX IF NOT EXISTS idx_outgoing_sends_sent_copy
ON outgoing_sends(status, sent_copy_status, sent_copy_next_attempt_at);

CREATE TABLE IF NOT EXISTS imap_draft_states (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    draft_key TEXT NOT NULL,
    local_message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    folder_id TEXT NOT NULL DEFAULT '',
    folder_remote_name TEXT NOT NULL,
    remote_uid INTEGER NOT NULL DEFAULT 0,
    uid_validity INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, draft_key)
);

CREATE TABLE IF NOT EXISTS imap_draft_operations (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL,
    draft_key TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('upsert', 'delete')),
    revision_token TEXT NOT NULL DEFAULT '',
    mime_data BLOB,
    message_date DATETIME,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'syncing', 'failed', 'ambiguous')),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    locked_at DATETIME,
    next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (account_id, draft_key) REFERENCES imap_draft_states(account_id, draft_key) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_imap_draft_operations_coalesced
ON imap_draft_operations(account_id, draft_key)
WHERE status IN ('pending', 'failed');

CREATE INDEX IF NOT EXISTS idx_imap_draft_operations_due
ON imap_draft_operations(status, next_attempt_at, created_at);

CREATE TABLE IF NOT EXISTS mail_security_exceptions (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('http_discovery', 'plaintext_transport', 'private_target')),
    protocol TEXT NOT NULL DEFAULT '',
    host TEXT NOT NULL CHECK (host <> ''),
    port INTEGER NOT NULL DEFAULT 0,
    created_by TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (
        (kind = 'http_discovery' AND protocol = '' AND port = 0)
        OR
        (kind = 'plaintext_transport' AND protocol IN ('imap', 'smtp') AND port BETWEEN 1 AND 65535)
        OR
        (kind = 'private_target' AND protocol IN ('http', 'https', 'imap', 'smtp') AND port BETWEEN 1 AND 65535)
    ),
    UNIQUE(kind, protocol, host, port)
);

CREATE INDEX IF NOT EXISTS idx_mail_security_exceptions_lookup
ON mail_security_exceptions(kind, protocol, host, port);

-- Schema version marker for fresh installs
INSERT OR REPLACE INTO schema_version (version) VALUES (72);
