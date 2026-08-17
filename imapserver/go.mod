// The server framework is a nested module, versioned v0.x independently of the
// root module's v1.x. SERVER-DESIGN.md §9 explains why; the short version is
// that the root API is frozen and this one is not, and one go.mod cannot carry
// two different promises.
//
// The require below is the one sanctioned exception to CLAUDE.md's
// zero-dependency rule: a self-referential dependency on our own root module,
// at a real released version rather than a replace directive. It is bumped
// deliberately on each root release this module wants to pick up.
//
// It must be a version that actually contains what this module imports.
// internal/imapcodec and internal/imapmessage landed after v1.0.0, so v1.1.0 is
// the floor — see docs/RELEASING.md for the ordering that implies.
module github.com/kiliant/go-imap/imapserver

go 1.24

require github.com/kiliant/go-imap v1.1.0
