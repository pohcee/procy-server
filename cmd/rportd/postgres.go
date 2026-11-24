package main

import (
	"context"
	"strings"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/storage/postgres/pgserver"
	"github.com/openrport/openrport/share/logger"
)

// notPostgresMsg ends the errors of the options that work only on a Postgres node.
const notPostgresMsg = `only with [storage] backend = "postgres"`

// postgresServerOpts opens the database of a Postgres node and returns the server options on it. close closes the
// database and what the options hold.
func postgresServerOpts(ctx context.Context, cfg *chconfig.Config, base *chserver.ServerOpts) (*chserver.ServerOpts, func(), error) {
	db, err := pgserver.Open(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	l := logger.NewLogger("postgres", cfg.Logging.LogOutput, cfg.Logging.LogLevel)
	opts, closeOpts, err := pgserver.NodeOpts(ctx, cfg, db, base, logWriter{l})
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return opts, func() {
		closeOpts()
		db.Close()
	}, nil
}

// logWriter writes each line it gets as an error to a logger.
type logWriter struct{ l *logger.Logger }

func (w logWriter) Write(p []byte) (int, error) {
	w.l.Errorf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
