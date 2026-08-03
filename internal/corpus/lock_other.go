//go:build !unix

package corpus

// lock is a no-op where there is no flock to call.
//
// Appends on such a platform are serialised only by whatever the caller does
// itself, which is the behaviour the whole package had before the lock existed.
// Keeping it a build-tagged no-op rather than a new module dependency means the
// package still compiles everywhere; the platforms talaria targets are all unix.
func lock(string) (func(), error) {
	return func() {}, nil
}
