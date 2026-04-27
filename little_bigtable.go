/*
bigtable-emulator-extended launches a PostgreSQL-backed Bigtable emulator on the given address.
*/
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"

	"github.com/jhsenjaliya/little_bigtable/bttest"
	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	"google.golang.org/grpc"
)

const (
	maxMsgSize = 256 * 1024 * 1024 // 256 MiB
	version    = "0.3.0-localcloud"
)

func main() {
	host := flag.String("host", "localhost", "the address to bind to on the local machine")
	port := flag.Int("port", 9000, "the port number to bind to on the local machine")
	databaseDriver := flag.String("database-driver", "postgres", "database/sql driver name: postgres or sqlite3")
	databaseURL := flag.String("database-url", "", "database/sql connection string")
	dbFile := flag.String("db-file", "little_bigtable.db", "legacy sqlite3 data file path")
	strictAdmin := flag.Bool("strict-admin", true, "require instances to exist before table/data APIs are used")
	showVersion := flag.Bool("version", false, "show version")

	ctx := context.Background()
	grpc.EnableTracing = false
	flag.Parse()
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	if *showVersion {
		fmt.Printf("bigtable-emulator-extended v%s (built w/%s)", version, runtime.Version())
		os.Exit(0)
	}

	if *databaseDriver == "" {
		log.Fatal("missing --database-driver")
	}

	dsn := *databaseURL
	if *databaseDriver == "sqlite3" {
		if *dbFile == "" {
			log.Fatal("missing --db-file")
		}
		dsn = fmt.Sprintf("file:%s?cache=shared", *dbFile)
	}
	if dsn == "" {
		log.Fatal("missing --database-url")
	}

	bttest.ConfigureStorage(*databaseDriver, *strictAdmin)

	db, err := sql.Open(*databaseDriver, dsn)
	if err != nil {
		log.Fatalf("failed creating database connection %v", err)
	}
	if *databaseDriver == "sqlite3" {
		db.SetMaxOpenConns(1)
	}

	err = bttest.CreateTables(ctx, db)
	if err != nil {
		log.Fatalf("%#v", err)
	}

	opts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(maxMsgSize),
		grpc.MaxSendMsgSize(maxMsgSize),
	}
	srv, err := bttest.NewServer(fmt.Sprintf("%s:%d", *host, *port), db, opts...)
	if err != nil {
		log.Fatalf("failed to start emulator: %v", err)
	}

	log.Printf("LocalCloud Bigtable emulator running. driver=%s strict_admin=%t BIGTABLE_EMULATOR_HOST=%q", *databaseDriver, *strictAdmin, srv.Addr)
	select {}
}
