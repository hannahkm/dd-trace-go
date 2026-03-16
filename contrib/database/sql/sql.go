// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

// Package sql provides functions to trace the database/sql package (https://golang.org/pkg/database/sql).
// It will automatically augment operations such as connections, statements and transactions with tracing.
//
// The recommended way to use this package is with the [Driver] function, which wraps a [database/sql/driver.Driver]
// with tracing and returns it for use with [database/sql.Register] and [database/sql.Open]:
//
//	tracedName, tracedDriver := sqltrace.Driver("postgres", &pq.Driver{})
//	sql.Register(tracedName, tracedDriver)
//	db, err := sql.Open(tracedName, "postgres://pqgotest:password@localhost...")
//
// Unlike the deprecated [Register]/[Open] functions, [Driver] does not use the driver name as the default
// service name. The service name defaults to the value of DD_SERVICE. To set a custom service name:
//
//	tracedName, tracedDriver := sqltrace.Driver("postgres", &pq.Driver{}, sqltrace.WithService("my-db"))
//
// For drivers that are configured programmatically via a [database/sql/driver.Connector] rather than a
// DSN string, use [Connector] instead:
//
//	tracedConnector := sqltrace.Connector("mysql", myConnector)
//	db := sql.OpenDB(tracedConnector)
package sql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	sqlinternal "github.com/DataDog/dd-trace-go/contrib/database/sql/v2/internal"

	"github.com/DataDog/dd-trace-go/v2/instrumentation"
	"github.com/DataDog/dd-trace-go/v2/instrumentation/env"
)

const componentName = instrumentation.PackageDatabaseSQL

var instr *instrumentation.Instrumentation

var (
	testMode         atomic.Bool
	testModeInitOnce sync.Once
)

func init() {
	instr = instrumentation.Load(instrumentation.PackageDatabaseSQL)
}

// registeredDrivers holds a registry of all drivers registered via the sqltrace package.
var registeredDrivers = &driverRegistry{
	keys:    make(map[reflect.Type]string),
	drivers: make(map[string]driver.Driver),
	configs: make(map[string]*config),
}

type driverRegistry struct {
	// keys maps driver types to their registered names.
	keys map[reflect.Type]string
	// drivers maps keys to their registered driver.
	drivers map[string]driver.Driver
	// configs maps keys to their registered configuration.
	configs map[string]*config
	// mu protects the above maps.
	mu sync.RWMutex
}

// isRegistered reports whether the name matches an existing entry
// in the driver registry.
func (d *driverRegistry) isRegistered(name string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.configs[name]
	return ok
}

// add adds the driver with the given name and config to the registry.
func (d *driverRegistry) add(name string, driver driver.Driver, cfg *config) {
	if d.isRegistered(name) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.keys[reflect.TypeOf(driver)] = name
	d.drivers[name] = driver
	d.configs[name] = cfg
}

// name returns the name of the driver stored in the registry.
func (d *driverRegistry) name(driver driver.Driver) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	name, ok := d.keys[reflect.TypeOf(driver)]
	return name, ok
}

// driver returns the driver stored in the registry with the provided name.
func (d *driverRegistry) driver(name string) (driver.Driver, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	driver, ok := d.drivers[name]
	return driver, ok
}

// config returns the config stored in the registry with the provided name.
func (d *driverRegistry) config(name string) (*config, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	config, ok := d.configs[name]
	return config, ok
}

// unregister is used to make tests idempotent.
func (d *driverRegistry) unregister(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	driver := d.drivers[name]
	delete(d.keys, reflect.TypeOf(driver))
	delete(d.configs, name)
	delete(d.drivers, name)
}

// Register tells the sql integration package about the driver that we will be tracing. If used, it
// must be called before Open. It uses the driverName suffixed with ".db" as the default service
// name.
//
// Deprecated: Register uses the driverName as the default service name, which couples service
// identity to the database driver. Use [Driver] instead, which does not derive the service name
// from the driver. To preserve the old service name after migrating, call:
//
//	sqltrace.Driver(driverName, myDriver, sqltrace.WithService(driverName+".db"))
func Register(driverName string, driver driver.Driver, opts ...Option) {
	if driver == nil {
		panic("sqltrace: Register driver is nil")
	}
	testModeInitOnce.Do(func() {
		_, ok := env.Lookup("__DD_TRACE_SQL_TEST")
		testMode.Store(ok)
	})
	testModeEnabled := testMode.Load()
	if registeredDrivers.isRegistered(driverName) {
		// already registered, don't change things
		if !testModeEnabled {
			return
		}
		// if we are in test mode, just unregister the driver and replace it
		unregister(driverName)
	}

	cfg := new(config)
	defaults(cfg, driverName, nil)
	processOptions(cfg, driverName, driver, "", opts...)
	instr.Logger().Debug("contrib/database/sql: Registering driver: %s %#v", driverName, cfg)
	registeredDrivers.add(driverName, driver, cfg)
}

// unregister is used to make tests idempotent.
func unregister(name string) {
	if registeredDrivers.isRegistered(name) {
		registeredDrivers.unregister(name)
	}
}

type tracedConnector struct {
	connector  driver.Connector
	driverName string
	cfg        *config
	dbClose    chan struct{}
}

func (t *tracedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	dsn := t.cfg.dsn
	if dc, ok := t.connector.(*dsnConnector); ok {
		dsn = dc.dsn
	}
	// check DBM propagation again, now that the DSN could be available.
	t.cfg.checkDBMPropagation(t.driverName, t.connector.Driver(), dsn)

	tp := &traceParams{
		driverName: t.driverName,
		cfg:        t.cfg,
	}
	if dsn != "" {
		tp.meta, _ = sqlinternal.ParseDSN(t.driverName, dsn)
	}
	start := time.Now()
	ctx, end := startTraceTask(ctx, string(QueryTypeConnect))
	defer end()
	conn, err := t.connector.Connect(ctx)
	tp.tryTrace(ctx, QueryTypeConnect, "", start, err)
	if err != nil {
		return nil, err
	}
	return &TracedConn{conn, tp}, err
}

func (t *tracedConnector) Driver() driver.Driver {
	return t.connector.Driver()
}

// Close closes the dbClose channel
// This method will be invoked when DB.Close() is called, which we expect to occur only once: https://cs.opensource.google/go/go/+/refs/tags/go1.23.4:src/database/sql/sql.go;l=918-950
func (t *tracedConnector) Close() error {
	close(t.dbClose)
	return nil
}

// from Go stdlib implementation of sql.Open
type dsnConnector struct {
	dsn    string
	driver driver.Driver
}

func (t dsnConnector) Connect(_ context.Context) (driver.Conn, error) {
	return t.driver.Open(t.dsn)
}

func (t dsnConnector) Driver() driver.Driver {
	return t.driver
}

// OpenDB returns connection to a DB using the traced version of the given driver. The driver may
// first be registered using Register. If this did not occur, OpenDB will determine the driver name
// based on its type.
//
// Deprecated: OpenDB derives the service name from the driver name or the driver's reflect type,
// which couples service identity to implementation details. Use [Connector] to wrap a
// [database/sql/driver.Connector] with tracing, then pass it to [database/sql.OpenDB]:
//
//	tracedConnector := sqltrace.Connector(driverName, myConnector)
//	db := sql.OpenDB(tracedConnector)
func OpenDB(c driver.Connector, opts ...Option) *sql.DB {
	cfg := new(config)
	var driverName string
	if name, ok := registeredDrivers.name(c.Driver()); ok {
		driverName = name
		rc, _ := registeredDrivers.config(driverName)
		defaults(cfg, driverName, rc)
	} else {
		driverName = reflect.TypeOf(c.Driver()).String()
		defaults(cfg, driverName, nil)
	}
	dsn := ""
	if dc, ok := c.(*dsnConnector); ok {
		dsn = dc.dsn
	}
	processOptions(cfg, driverName, c.Driver(), dsn, opts...)
	tc := &tracedConnector{
		connector:  c,
		driverName: driverName,
		cfg:        cfg,
		dbClose:    make(chan struct{}),
	}
	db := sql.OpenDB(tc)
	if cfg.dbStats && cfg.statsdClient != nil {
		go pollDBStats(cfg.statsdClient, db, tc.dbClose)
	}
	return db
}

// Open returns connection to a DB using the traced version of the given driver. The driver may
// first be registered using Register. If this did not occur, Open will determine the driver by
// opening a DB connection and retrieving the driver using (*sql.DB).Driver, before closing it and
// opening a new, traced connection.
//
// Deprecated: Open relies on [Register] and inherits its service-name behavior, using the
// driverName as the default service name. Use [Driver] to obtain a traced driver, then call
// [database/sql.Open] directly:
//
//	tracedName, tracedDriver := sqltrace.Driver(driverName, myDriver)
//	sql.Register(tracedName, tracedDriver)
//	db, err := sql.Open(tracedName, dsn)
func Open(driverName, dataSourceName string, opts ...Option) (*sql.DB, error) {
	var d driver.Driver
	if registeredDrivers.isRegistered(driverName) {
		d, _ = registeredDrivers.driver(driverName)
	} else {
		db, err := sql.Open(driverName, dataSourceName)
		if err != nil {
			return nil, err
		}
		defer db.Close()
		d = db.Driver()
		Register(driverName, d)
	}

	if driverCtx, ok := d.(driver.DriverContext); ok {
		connector, err := driverCtx.OpenConnector(dataSourceName)
		if err != nil {
			return nil, err
		}
		// since we're not using the dsnConnector, we need to register the dsn manually in the config
		optsCopy := make([]Option, len(opts))
		copy(optsCopy, opts) // avoid modifying the provided opts, so make a copy instead, and use this
		optsCopy = append(optsCopy, WithDSN(dataSourceName))
		return OpenDB(connector, optsCopy...), nil
	}
	return OpenDB(&dsnConnector{dsn: dataSourceName, driver: d}, opts...), nil
}

func processOptions(cfg *config, driverName string, driver driver.Driver, dsn string, opts ...Option) {
	for _, fn := range opts {
		fn.apply(cfg)
	}
	cfg.checkDBMPropagation(driverName, driver, dsn)
	cfg.checkStatsdRequired()
}

// driverSeq is an atomically incremented counter used to generate unique driver names.
var driverSeq atomic.Int64

// Driver wraps the given driver with tracing and returns a unique name along with
// the traced driver. The returned values can be used with [database/sql.Register]
// and [database/sql.Open]:
//
//	tracedName, tracedDriver := sqltrace.Driver("postgres", &pq.Driver{})
//	sql.Register(tracedName, tracedDriver)
//	db, err := sql.Open(tracedName, dsn)
//
// The driverName is used for span metadata (span name, db.system tag, DSN parsing) but,
// unlike [Register], it is NOT used as the default service name. The service name defaults
// to the value of DD_SERVICE. To set a custom service name, use [WithService]:
//
//	sqltrace.Driver("postgres", &pq.Driver{}, sqltrace.WithService("my-db"))
//
// To restore the legacy behavior where the driver name was used as the service name:
//
//	sqltrace.Driver("postgres", &pq.Driver{}, sqltrace.WithService("postgres.db"))
func Driver(driverName string, d driver.Driver, opts ...Option) (string, driver.Driver) {
	if d == nil {
		panic("sqltrace: Driver driver is nil")
	}
	cfg := new(config)
	defaultsWithoutDriverService(cfg, driverName)
	processOptions(cfg, driverName, d, "", opts...)
	instr.Logger().Debug("contrib/database/sql: Wrapping driver: %s %#v", driverName, cfg)

	seq := driverSeq.Add(1)
	tracedName := fmt.Sprintf("%s-traced-%d", driverName, seq)

	td := &tracedDriver{
		driver:     d,
		driverName: driverName,
		cfg:        cfg,
	}
	return tracedName, td
}

// tracedDriver wraps a driver.Driver with tracing. It implements both driver.Driver and
// driver.DriverContext so that database/sql always goes through the traced connector path.
type tracedDriver struct {
	driver     driver.Driver
	driverName string
	cfg        *config
}

var _ driver.Driver = (*tracedDriver)(nil)
var _ driver.DriverContext = (*tracedDriver)(nil)

// Open implements driver.Driver.
func (td *tracedDriver) Open(dsn string) (driver.Conn, error) {
	tp := &traceParams{
		driverName: td.driverName,
		cfg:        td.cfg,
	}
	if dsn != "" {
		tp.meta, _ = sqlinternal.ParseDSN(td.driverName, dsn)
	}
	start := time.Now()
	conn, err := td.driver.Open(dsn)
	tp.tryTrace(context.Background(), QueryTypeConnect, "", start, err)
	if err != nil {
		return nil, err
	}
	return &TracedConn{conn, tp}, nil
}

// OpenConnector implements driver.DriverContext. It returns a traced connector
// that wraps each connection with tracing.
func (td *tracedDriver) OpenConnector(dsn string) (driver.Connector, error) {
	var connector driver.Connector
	if dc, ok := td.driver.(driver.DriverContext); ok {
		var err error
		connector, err = dc.OpenConnector(dsn)
		if err != nil {
			return nil, err
		}
	} else {
		connector = &dsnConnector{dsn: dsn, driver: td.driver}
	}
	cfgCopy := *td.cfg
	cfgCopy.dsn = dsn
	return &tracedConnector{
		connector:  connector,
		driverName: td.driverName,
		cfg:        &cfgCopy,
		dbClose:    make(chan struct{}),
	}, nil
}

// Connector wraps an existing [database/sql/driver.Connector] with tracing and returns it
// for use with [database/sql.OpenDB]. This is the connector-based counterpart to [Driver],
// intended for cases where the driver is configured programmatically via a connector rather
// than a DSN string:
//
//	connector, err := mysql.NewConnector(cfg)
//	tracedConnector := sqltrace.Connector("mysql", connector)
//	db := sql.OpenDB(tracedConnector)
//
// The driverName is used for span metadata (span name, db.system tag, DSN parsing) but,
// like [Driver], it is NOT used as the default service name. The service name defaults
// to the value of DD_SERVICE. To set a custom service name, use [WithService]:
//
//	sqltrace.Connector("mysql", connector, sqltrace.WithService("my-db"))
//
// If the connector's DSN is needed for span tags (e.g. db.instance, peer.hostname),
// use [WithDSN] to provide it:
//
//	sqltrace.Connector("mysql", connector, sqltrace.WithDSN("user:pass@tcp(host)/db"))
func Connector(driverName string, c driver.Connector, opts ...Option) driver.Connector {
	if c == nil {
		panic("sqltrace: Connector connector is nil")
	}
	cfg := new(config)
	defaultsWithoutDriverService(cfg, driverName)
	processOptions(cfg, driverName, c.Driver(), cfg.dsn, opts...)
	instr.Logger().Debug("contrib/database/sql: Wrapping connector: %s %#v", driverName, cfg)

	tc := &tracedConnector{
		connector:  c,
		driverName: driverName,
		cfg:        cfg,
		dbClose:    make(chan struct{}),
	}
	return tc
}
