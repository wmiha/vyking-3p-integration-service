// Command seed fills a local database with deterministic sample data for
// manual and end-to-end testing: well-known fixture customers covering the
// task's edge cases plus random customers, transactions and sync entries.
//
// It writes tables the service itself may not write, so it connects as the
// MySQL root user of the local compose stack. It refuses to run against
// non-local hosts unless -force is given.
//
//	go run ./scripts/seed -reset
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/platform/database"
)

var localHosts = []string{"127.0.0.1", "localhost", "::1", "mysql"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		host     = flag.String("host", env("SEED_DB_HOST", "127.0.0.1"), "MySQL host")
		port     = flag.Int("port", 3306, "MySQL port")
		name     = flag.String("db", env("DB_NAME", "vyking"), "database name")
		user     = flag.String("user", "root", "MySQL user (needs write access to every table)")
		password = flag.String("password", env("MYSQL_ROOT_PASSWORD", "root-dev-only"), "MySQL password")

		customers  = flag.Int("customers", 200, "random customers in addition to the fixtures")
		maxTx      = flag.Int("tx", 30, "max transactions per random customer")
		queueShare = flag.Float64("queue", 0.3, "share of random customers with outstanding sync entries")
		seed       = flag.Uint64("seed", 42, "random seed (same seed, same data)")
		reset      = flag.Bool("reset", false, "delete existing non-reference data first")
		force      = flag.Bool("force", false, "allow non-local hosts")
	)
	flag.Parse()

	if !*force && !slices.Contains(localHosts, *host) {
		return fmt.Errorf("refusing to seed non-local host %q (use -force if you really mean it)", *host)
	}

	db, err := database.Open(database.Config{
		Host: *host, Port: *port, Name: *name, User: *user, Password: *password, Timezone: "UTC",
		ConnectTimeout: 5 * time.Second, ReadTimeout: time.Minute, WriteTimeout: time.Minute,
		MaxOpenConns: 2, MaxIdleConns: 1,
	})
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := database.WaitReady(ctx, db, time.Second); err != nil {
		return err
	}

	ds := generate(options{
		customers:  *customers,
		maxTx:      *maxTx,
		queueShare: *queueShare,
		seed:       *seed,
		now:        time.Now().UTC().Truncate(time.Second),
	})

	// Everything or nothing: a failed seed never leaves a half-filled schema.
	err = database.WithTx(ctx, db, func(tx *sql.Tx) error {
		if *reset {
			if err := deleteAll(ctx, tx); err != nil {
				return err
			}
		}
		return write(ctx, tx, ds)
	})
	if err != nil {
		return err
	}

	fmt.Printf("seeded %s: %d brands, %d payment methods, %d customers, %d transactions, %d sync_queue entries, %d partner contacts\n",
		*name, len(ds.brands), len(ds.paymentMethods), len(ds.customers), len(ds.transactions),
		len(ds.syncQueue), len(ds.partnerContacts))
	return nil
}

// dataTables are emptied by -reset, children first. Reference tables
// (types, statuses, currencies) belong to the migrations and are kept.
var dataTables = []string{"partner_contacts", "sync_queue", "transactions", "customers", "payment_methods", "brands"}

func deleteAll(ctx context.Context, tx *sql.Tx) error {
	for _, t := range dataTables {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("reset %s: %w", t, err)
		}
	}
	return nil
}

func write(ctx context.Context, tx *sql.Tx, ds dataset) error {
	inserts := []struct {
		table string
		cols  []string
		rows  []row
	}{
		{"brands", []string{"BRD_iID", "BRD_cName"}, ds.brands},
		{"payment_methods", []string{"PMT_iID", "PMT_cName"}, ds.paymentMethods},
		{"customers", []string{"CUS_iID", "CUS_BRD_iID", "CUS_cUserName", "CUS_cEmail", "CUS_cFirstName",
			"CUS_cLastName", "CUS_cPhone", "CUS_cCountry", "CUS_CUR_iID", "CUS_dCreated", "CUS_bActive"}, ds.customers},
		{"transactions", []string{"TRX_iID", "TRX_CUS_iID", "TRX_TYP_iID", "TRX_STA_iID", "TRX_iAmount",
			"TRX_PMT_iID", "TRX_dCreated", "TRX_dBooked", "TRX_dChanged"}, ds.transactions},
		{"sync_queue", []string{"SYQ_iID", "SYQ_CUS_iID", "SYQ_dQueued", "SYQ_dProcessed"}, ds.syncQueue},
		{"partner_contacts", []string{"PCO_CUS_iID", "PCO_cContactId", "PCO_dSynced"}, ds.partnerContacts},
	}
	for _, in := range inserts {
		if err := insertRows(ctx, tx, in.table, in.cols, in.rows); err != nil {
			return err
		}
	}
	return nil
}

// insertRows writes rows with multi-row INSERT statements of bounded size.
// Table and column names are compile-time constants; values are bound.
func insertRows(ctx context.Context, tx *sql.Tx, table string, cols []string, rows []row) error {
	const batch = 500
	tuple := "(" + strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + ")"

	for start := 0; start < len(rows); start += batch {
		chunk := rows[start:min(start+batch, len(rows))]
		query := fmt.Sprintf("INSERT INTO %s (%s) VALUES %s", table, strings.Join(cols, ", "),
			strings.TrimSuffix(strings.Repeat(tuple+",", len(chunk)), ","))

		args := make([]any, 0, len(chunk)*len(cols))
		for _, r := range chunk {
			args = append(args, r...)
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("insert into %s: %w", table, err)
		}
	}
	return nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
