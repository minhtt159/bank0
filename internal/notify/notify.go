// Package notify is the out-of-band delivery seam for verification codes.
// v1 ships only LogNotifier (dev: the code appears in the app log); a real
// email/SMS provider implements Notifier without touching the handlers.
// The code is never persisted in plaintext and never returned over the API.
package notify

import (
	"context"
	"log/slog"
)

type Notifier interface {
	SendVerification(ctx context.Context, channel, destination, code string) error
}

// LogNotifier logs dispatches. The code itself is included only when logCodes
// is true (wired to env != production) so production logs never carry secrets.
type LogNotifier struct {
	Log      *slog.Logger
	LogCodes bool
}

func (n LogNotifier) SendVerification(_ context.Context, channel, destination, code string) error {
	if n.LogCodes {
		n.Log.Info("verification code dispatched", "channel", channel, "destination", destination, "code", code)
	} else {
		n.Log.Info("verification code dispatched", "channel", channel, "destination", destination)
	}
	return nil
}
