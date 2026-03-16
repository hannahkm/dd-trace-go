// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package sql_test

import (
	"context"
	"database/sql"
	"log"

	sqltrace "github.com/DataDog/dd-trace-go/contrib/database/sql/v2"

	"github.com/DataDog/dd-trace-go/v2/ddtrace/ext"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"

	"github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
	sqlite "github.com/mattn/go-sqlite3" // Setup application to use Sqlite
)

func Example() {
	// The first step is to obtain a traced driver using sqltrace.Driver.
	// The service name defaults to DD_SERVICE and is NOT derived from the driver name.
	tracedName, tracedDriver := sqltrace.Driver("postgres", &pq.Driver{})
	sql.Register(tracedName, tracedDriver)

	// Followed by a call to sql.Open with the traced driver name.
	db, err := sql.Open(tracedName, "postgres://pqgotest:password@localhost/pqgotest?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}

	// Then, we continue using the database/sql package as we normally would, with tracing.
	rows, err := db.Query("SELECT name FROM users WHERE age=?", 27)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
}

func Example_withService() {
	tracer.Start()
	defer tracer.Stop()

	// Use WithService to set a custom service name for the traced driver.
	tracedName, tracedDriver := sqltrace.Driver("mysql", &mysql.MySQLDriver{}, sqltrace.WithService("my-db"))
	sql.Register(tracedName, tracedDriver)

	// Open a connection to the DB using the traced driver.
	db, err := sql.Open(tracedName, "user:password@/dbname")
	if err != nil {
		log.Fatal(err)
	}

	// Create a root span, giving name, server and resource.
	span, ctx := tracer.StartSpanFromContext(context.Background(), "my-query",
		tracer.SpanType(ext.SpanTypeSQL),
		tracer.ServiceName("my-db"),
		tracer.ResourceName("initial-access"),
	)

	// Subsequent spans inherit their parent from context.
	rows, err := db.QueryContext(ctx, "SELECT * FROM city LIMIT 5")
	if err != nil {
		log.Fatal(err)
	}
	rows.Close()
	span.Finish(tracer.WithError(err))
}

func Example_legacyServiceName() {
	// To restore the legacy behavior where the driver name becomes the service name
	// (e.g. "postgres.db"), explicitly pass it via WithService.
	tracedName, tracedDriver := sqltrace.Driver("postgres", &pq.Driver{}, sqltrace.WithService("postgres.db"))
	sql.Register(tracedName, tracedDriver)

	db, err := sql.Open(tracedName, "postgres://pqgotest:password@localhost/pqgotest?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}

	rows, err := db.Query("SELECT name FROM users WHERE age=?", 27)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
}

func Example_sqlite() {
	// Wrap the Sqlite driver with tracing and a custom service name.
	tracedName, tracedDriver := sqltrace.Driver("sqlite", &sqlite.SQLiteDriver{}, sqltrace.WithService("sqlite-example"))
	sql.Register(tracedName, tracedDriver)

	// Open a connection to the DB using the traced driver.
	db, err := sql.Open(tracedName, "./test.db")
	if err != nil {
		log.Fatal(err)
	}

	// Create a root span, giving name, server and resource.
	span, ctx := tracer.StartSpanFromContext(context.Background(), "my-query",
		tracer.SpanType("example"),
		tracer.ServiceName("sqlite-example"),
		tracer.ResourceName("initial-access"),
	)

	// Subsequent spans inherit their parent from context.
	rows, err := db.QueryContext(ctx, "SELECT * FROM city LIMIT 5")
	if err != nil {
		log.Fatal(err)
	}
	rows.Close()
	span.Finish(tracer.WithError(err))
}

func Example_connector() {
	// When a driver is configured programmatically via a Connector (instead of a DSN string),
	// use sqltrace.Connector to wrap it with tracing.
	mysqlCfg := mysql.NewConfig()
	mysqlCfg.User = "root"
	mysqlCfg.Net = "tcp"
	mysqlCfg.Addr = "localhost:3306"
	mysqlCfg.DBName = "mydb"
	connector, err := mysql.NewConnector(mysqlCfg)
	if err != nil {
		log.Fatal(err)
	}

	tracedConnector := sqltrace.Connector("mysql", connector, sqltrace.WithService("my-db"))
	db := sql.OpenDB(tracedConnector)

	rows, err := db.Query("SELECT name FROM users WHERE age=?", 27)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
}

func Example_dbmPropagation() {
	// Enable DBM propagation when creating the traced driver.
	tracedName, tracedDriver := sqltrace.Driver("postgres", &pq.Driver{}, sqltrace.WithDBMPropagation(tracer.DBMPropagationModeFull))
	sql.Register(tracedName, tracedDriver)

	// Followed by a call to sql.Open.
	db, err := sql.Open(tracedName, "postgres://pqgotest:password@localhost/pqgotest?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}

	// Then, we continue using the database/sql package as we normally would, with tracing.
	rows, err := db.Query("SELECT name FROM users WHERE age=?", 27)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
}

func Example_dbStats() {
	// Enable DBStats metric polling when creating the traced driver.
	tracedName, tracedDriver := sqltrace.Driver("postgres", &pq.Driver{}, sqltrace.WithDBStats())
	sql.Register(tracedName, tracedDriver)

	// Followed by a call to sql.Open.
	db, err := sql.Open(tracedName, "postgres://pqgotest:password@localhost/pqgotest?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}

	// Tracing and metric polling is now enabled. Metrics will be submitted to Datadog with the prefix `datadog.tracer.sql`
	rows, err := db.Query("SELECT name FROM users WHERE age=?", 27)
	if err != nil {
		log.Fatal(err)
	}
	rows.Close()
}
