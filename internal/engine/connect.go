package engine

import (
	"context"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/imapsource"
	"github.com/michele/imapvault/internal/mail"
)

// Dial connects to the account's IMAP server. The returned source is read-only.
// The caller closes it.
func Dial(ctx context.Context, cfg *config.Config) (mail.Source, error) {
	return imapsource.Dial(ctx, imapsource.Options{
		Host:     cfg.IMAP.Host,
		Port:     cfg.IMAP.Port,
		TLS:      cfg.IMAP.TLS,
		Username: cfg.IMAP.Username,
		Password: cfg.IMAP.Password,
	})
}
