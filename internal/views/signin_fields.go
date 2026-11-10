package views

// The text fields of the pages shown before sign-in, shared by the webmail
// and admin pages so both look and behave alike. aria-invalid marks the field
// an error message is about.
const (
	signInFieldClass     = "flex h-10 w-full rounded-md border border-input bg-background px-3 py-2 text-sm outline-none transition-shadow placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/50 aria-[invalid=true]:border-destructive aria-[invalid=true]:ring-destructive/20"
	signInCodeFieldClass = "flex h-12 w-full rounded-md border border-input bg-background px-3 py-2 text-center font-mono text-xl tracking-[0.35em] outline-none transition-shadow placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/50 aria-[invalid=true]:border-destructive aria-[invalid=true]:ring-destructive/20"
)
