// Command devdb runs a throwaway Postgres for trying rportd on Postgres locally, without Docker: it starts an
// embedded Postgres, migrates it, creates the application role rport_app and prints a [storage] section for
// rportd.conf, then waits until interrupted. The data is deleted when it stops. Development only:
//
//	go run ./server/storage/postgres/pgtest/devdb
package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/migrate"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "devdb:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopDB, adminURL, err := pgtest.StartEmbedded()
	if err != nil {
		return err
	}
	defer stopDB()
	if err := pgtest.EnsureAppRole(adminURL); err != nil {
		return err
	}
	owner, err := postgres.Open(ctx, adminURL)
	if err != nil {
		return err
	}
	defer owner.Close()
	if _, err := migrate.Up(ctx, owner); err != nil {
		return err
	}
	if err := migrate.GrantApp(ctx, owner, pgtest.AppRole); err != nil {
		return err
	}
	u, _ := url.Parse(adminURL)
	u.User = url.UserPassword(pgtest.AppRole, pgtest.AppRole)
	fmt.Printf("[storage]\n  backend = \"postgres\"\n  dsn = %q\n\n", u.String())
	fmt.Printf("# schema owner (rportd migrate/tenant add/import --dsn): %s\n", adminURL)
	fmt.Println("# add a user: rportd user add -c rportd.conf -u admin -g Administrators")
	fmt.Println("# Ctrl-C stops the database and deletes its data")
	<-ctx.Done()
	return nil
}
