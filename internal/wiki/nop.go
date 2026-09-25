package wiki

import "context"

// Nop stands in when WIKI_ENABLED is off, so plugin.Context.Wiki is never
// nil and every use can ask Enabled rather than guard against a nil
// interface.
type Nop struct{}

func (Nop) Enabled() bool { return false }
func (Nop) Lookup(context.Context, string, string) (string, error) {
	return "", ErrUnavailable
}
