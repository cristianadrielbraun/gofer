# gofer.email

Small local email client thing I am building for myself.

It is very much a work in progress. Stuff is missing, some things are messy, and I am still figuring out the shape of it.

Right now it is a Go app with templ views, HTMX-ish interactions, SQLite storage, and IMAP/SMTP support. The idea is to have a fast local mail client that stores mail locally and talks directly to normal mail servers.

## current state

Some things work:

- adding IMAP/SMTP accounts
- syncing mail into SQLite
- reading messages
- sending mail
- basic folders / unread / starred state
- local cached message bodies and attachments

Some things are probably broken or half done. There are basically no tests yet. Do not run this on a public server.

## running it

You need Go, `templ`, `tailwindcss`, and `task` if you want to use the task commands.

```sh
task dev
```

Or roughly:

```sh
templ generate
tailwindcss -i ./assets/css/input.css -o ./assets/css/output.css
go run .
```

Then open `http://localhost:8090`.

## data

Runtime data lives in `data/` by default. That includes the SQLite DB, cached emails, attachments, and the local secret key used for encrypted account passwords.

That directory is ignored by git. Do not commit it.

Useful env vars:

```sh
GO_ENV=development
GOFER_DB_PATH=data/gofer.db
GOFER_SECRET_KEY=64_hex_chars_if_you_want_to_provide_your_own_key
```

## warning

This is a personal WIP project, not a finished mail client. It has sharp edges and the security model is basically "run it locally and don't expose it".

If you try it anyway, expect weirdness.
