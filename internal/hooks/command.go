package hooks

// ManagedCommand is the CLI command provider hooks should invoke by default.
// It intentionally uses the bare executable name so hook configs survive moves
// between install locations like Homebrew, curl installs, and local rebuilds.
const ManagedCommand = "semantica"
