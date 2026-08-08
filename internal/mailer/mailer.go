// Package mailer delivers the one-time codes institutional verification sends.
//
// Sender is the seam, in the same way internal/media.Store is the seam for blob
// storage: the development implementation prints the code instead of sending
// it, so the feature works end to end with no provider configured, and an SMTP
// or Google Workspace sender is another implementation of this interface rather
// than a change to any handler.
//
// Nothing here persists anything. A Code exists for the length of one SendCode
// call; educhan itself stores only an HMAC of the address it went to.
package mailer

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// Code is a one-time verification code on its way to one recipient.
type Code struct {
	// Address is the institutional address that asked to be verified. A Sender
	// may use it to deliver the message and must not retain it.
	Address string
	// Code is the value the recipient types back into the verification form.
	Code string
	// TTL is how long the code stays valid, for the message to quote.
	TTL time.Duration
}

// Sender delivers a verification code to its recipient.
//
// An implementation should treat delivery as best effort from the caller's
// point of view: educhan stores the pending code before sending, so a failure
// here costs the user a retry rather than leaving a code nobody can use.
type Sender interface {
	SendCode(ctx context.Context, code Code) error
}

// Dev is the Sender used when no provider is configured: it writes the code to
// an io.Writer — os.Stdout in the server — instead of emailing it.
//
// It prints the recipient's address, which is the one place in educhan an
// address is rendered at all, so it is a development tool only. Configure a
// real Sender before running the feature anywhere with users on it.
type Dev struct {
	mu  sync.Mutex
	out io.Writer
}

// NewDev returns a development sender writing to out.
func NewDev(out io.Writer) *Dev { return &Dev{out: out} }

// SendCode prints the code. Writes are serialised so two concurrent
// verifications cannot interleave into one unreadable line.
func (d *Dev) SendCode(_ context.Context, code Code) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := fmt.Fprintf(d.out, "[educhan] verification code for %s: %s (valid for %s)\n",
		code.Address, code.Code, code.TTL.Round(time.Second))
	return err
}
