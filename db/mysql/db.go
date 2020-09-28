// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/pingcap/go-ycsb/pkg/util"

	// mysql package
	_ "github.com/go-sql-driver/mysql"
	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
)

// mysql properties
const (
	mysqlHost       = "mysql.host"
	mysqlPort       = "mysql.port"
	mysqlUser       = "mysql.user"
	mysqlPassword   = "mysql.password"
	mysqlDBName     = "mysql.db"
	mysqlForceIndex = "mysql.force_index"
	// TODO: support batch and auto commit
)

type mysqlCreator struct {
}

type mysqlDB struct {
	p                 *properties.Properties
	db                *sql.DB
	verbose           bool
	forceIndexKeyword string

	bufPool *util.BufPool
}

type contextKey string

const stateKey = contextKey("mysqlDB")

type mysqlState struct {
	// Do we need a LRU cache here?
	stmtCache map[string]*sql.Stmt

	conn *sql.Conn
}

func (c mysqlCreator) Create(p *properties.Properties) (ycsb.DB, error) {
	d := new(mysqlDB)
	d.p = p

	host := p.GetString(mysqlHost, "127.0.0.1")
	port := p.GetInt(mysqlPort, 3306)
	user := p.GetString(mysqlUser, "root")
	password := p.GetString(mysqlPassword, "")
	dbName := p.GetString(mysqlDBName, "test")

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", user, password, host, port, dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}

	threadCount := int(p.GetInt64(prop.ThreadCount, prop.ThreadCountDefault))
	if p.GetBool(prop.UseShortConn, prop.UseShortConnDefault) {
		// Unlimited max open to avoid reusing returned conn
		db.SetMaxOpenConns(0)
		// No idle conn, every conn is closed when returned to the pool
		db.SetMaxIdleConns(-1)
	} else {
		db.SetMaxIdleConns(threadCount + 1)
		db.SetMaxOpenConns(threadCount * 2)
	}
	d.db = db

	d.verbose = p.GetBool(prop.Verbose, prop.VerboseDefault)
	if p.GetBool(mysqlForceIndex, true) {
		d.forceIndexKeyword = "FORCE INDEX(`PRIMARY`)"
	}

	d.bufPool = util.NewBufPool()

	if err := d.createTable(); err != nil {
		return nil, err
	}

	return d, nil
}

func (db *mysqlDB) getDB() (*sql.DB, error) {
	//p := db.p
	//if p.GetBool(prop.UseShortConn, prop.UseShortConnDefault) {
	//	host := p.GetString(mysqlHost, "127.0.0.1")
	//	port := p.GetInt(mysqlPort, 3306)
	//	user := p.GetString(mysqlUser, "root")
	//	password := p.GetString(mysqlPassword, "")
	//	dbName := p.GetString(mysqlDBName, "test")
	//
	//	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", user, password, host, port, dbName)
	//	myDB, err := sql.Open("mysql", dsn)
	//	if err != nil {
	//		return nil, err
	//	}
	//	myDB.SetMaxOpenConns(1)
	//	return myDB, nil
	//}
	return db.db, nil
}

func (db *mysqlDB) createTable() error {
	tableName := db.p.GetString(prop.TableName, prop.TableNameDefault)

	if db.p.GetBool(prop.DropData, prop.DropDataDefault) && !db.p.GetBool(prop.DoTransactions, true) {
		myDB, err := db.getDB()
		if err != nil {
			return err
		}
		if _, err := myDB.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", tableName)); err != nil {
			return err
		}
	}

	fieldCount := db.p.GetInt64(prop.FieldCount, prop.FieldCountDefault)
	fieldLength := db.p.GetInt64(prop.FieldLength, prop.FieldLengthDefault)
	fields := db.p.GetString(prop.Fields, prop.FieldsDefault)

	buf := new(bytes.Buffer)
	s := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (YCSB_KEY VARCHAR(64) PRIMARY KEY", tableName)
	buf.WriteString(s)
	if fields == "" {
		for i := int64(0); i < fieldCount; i++ {
			buf.WriteString(fmt.Sprintf(", FIELD%d VARCHAR(%d)", i, fieldLength))
		}
	} else {
		var genFields []byte
		genFields, err := util.GenerateFields(fields)
		if err != nil {
			return err
		}
		buf.Write(genFields)
	}

	buf.WriteString(");")

	if db.verbose {
		fmt.Println(buf.String())
	}

	myDB, err := db.getDB()
	if err != nil {
		return err
	}
	_, err = myDB.Exec(buf.String())
	return err
}

func (db *mysqlDB) Close() error {
	if db.db == nil {
		return nil
	}

	return db.db.Close()
}

func (db *mysqlDB) InitThread(ctx context.Context, _ int, _ int) context.Context {
	conn, err := db.db.Conn(ctx)
	if err != nil {
		panic(fmt.Sprintf("failed to create db conn %v", err))
	}

	state := &mysqlState{
		stmtCache: make(map[string]*sql.Stmt),
		conn:      conn,
	}

	if db.p.GetBool(prop.UseShortConn, prop.UseShortConnDefault) {
		conn.Close()
	}

	return context.WithValue(ctx, stateKey, state)
}

func (db *mysqlDB) CleanupThread(ctx context.Context) {
	state := ctx.Value(stateKey).(*mysqlState)

	for _, stmt := range state.stmtCache {
		stmt.Close()
	}
	state.conn.Close()
}

func (db *mysqlDB) getAndCacheStmt(ctx context.Context, query string) (*sql.Stmt, error) {
	state := ctx.Value(stateKey).(*mysqlState)

	if stmt, ok := state.stmtCache[query]; ok {
		return stmt, nil
	}

	stmt, err := state.conn.PrepareContext(ctx, query)
	if err == sql.ErrConnDone {
		// Try build the connection and prepare again
		if state.conn, err = db.db.Conn(ctx); err == nil {
			stmt, err = state.conn.PrepareContext(ctx, query)
		}
	}

	if err != nil {
		return nil, err
	}

	state.stmtCache[query] = stmt
	return stmt, nil
}

func (db *mysqlDB) execContextInNewConn(ctx context.Context, query string, args ...interface{}) error {
	conn, err := db.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := conn.Close(); err != nil && !db.p.GetBool(prop.Silence, prop.SilenceDefault){
			fmt.Printf("err closing mysql connection: %v\n", err)
		}
	}()
	_, err = conn.ExecContext(ctx, query, args...)
	return err
}

func (db *mysqlDB) clearCacheIfFailed(ctx context.Context, query string, err error) {
	if err == nil {
		return
	}
	//
	//state := ctx.Value(stateKey).(*mysqlState)
	//if stmt, ok := state.stmtCache[query]; ok {
	//	stmt.Close()
	//}
	//delete(state.stmtCache, query)
}

func (db *mysqlDB) queryRows(ctx context.Context, query string, count int, args ...interface{}) ([]map[string][]byte, error) {
	if db.verbose {
		fmt.Printf("%s %v\n", query, args)
	}

	var rows *sql.Rows
	var err error
	var conn *sql.Conn
	if !db.p.GetBool(prop.UseShortConn, prop.UseShortConnDefault) {
		var stmt *sql.Stmt
		stmt, err = db.getAndCacheStmt(ctx, query)
		if err != nil {
			return nil, err
		}
		rows, err = stmt.QueryContext(ctx, args...)
	} else {
		conn, err = db.db.Conn(ctx)
		if err != nil {
			return nil, err
		}
		rows, err = conn.QueryContext(ctx, query, args...)
		defer conn.Close()
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	vs := make([]map[string][]byte, 0, count)
	for rows.Next() {
		m := make(map[string][]byte, len(cols))
		dest := make([]interface{}, len(cols))
		for i := 0; i < len(cols); i++ {
			v := new([]byte)
			dest[i] = v
		}
		if err = rows.Scan(dest...); err != nil {
			return nil, err
		}

		for i, v := range dest {
			m[cols[i]] = *v.(*[]byte)
		}

		vs = append(vs, m)
	}

	return vs, rows.Err()
}

func (db *mysqlDB) Read(ctx context.Context, table string, key string, fields []string) (map[string][]byte, error) {
	var query string
	if len(fields) == 0 {
		query = fmt.Sprintf(`SELECT * FROM %s %s WHERE YCSB_KEY = ?`, table, db.forceIndexKeyword)
	} else {
		query = fmt.Sprintf(`SELECT %s FROM %s %s WHERE YCSB_KEY = ?`, strings.Join(fields, ","), table, db.forceIndexKeyword)
	}

	rows, err := db.queryRows(ctx, query, 1, key)
	if !db.p.GetBool(prop.UseShortConn, prop.UseShortConnDefault) {
		db.clearCacheIfFailed(ctx, query, err)
	}

	if err != nil {
		return nil, err
	} else if len(rows) == 0 {
		return nil, nil
	}

	return rows[0], nil
}

func (db *mysqlDB) Scan(ctx context.Context, table string, startKey string, count int, fields []string) ([]map[string][]byte, error) {
	var query string
	if len(fields) == 0 {
		query = fmt.Sprintf(`SELECT * FROM %s %s WHERE YCSB_KEY >= ? LIMIT ?`, table, db.forceIndexKeyword)
	} else {
		query = fmt.Sprintf(`SELECT %s FROM %s %s WHERE YCSB_KEY >= ? LIMIT ?`, strings.Join(fields, ","), table, db.forceIndexKeyword)
	}

	rows, err := db.queryRows(ctx, query, count, startKey, count)
	if !db.p.GetBool(prop.UseShortConn, prop.UseShortConnDefault) {
		db.clearCacheIfFailed(ctx, query, err)
	}

	return rows, err
}

func (db *mysqlDB) execQuery(ctx context.Context, query string, args ...interface{}) error {
	if db.verbose {
		fmt.Printf("%s %v\n", query, args)
	}

	var err error
	if db.p.GetBool(prop.UseShortConn, prop.UseShortConnDefault) {
		err = db.execContextInNewConn(ctx, query, args...)
	} else {
		var stmt *sql.Stmt
		stmt, err = db.getAndCacheStmt(ctx, query)
		if err != nil {
			return err
		}
		_, err = stmt.ExecContext(ctx, args...)
		db.clearCacheIfFailed(ctx, query, err)
	}
	return err
}

func (db *mysqlDB) Update(ctx context.Context, table string, key string, values map[string][]byte) error {
	buf := db.bufPool.Get()
	defer db.bufPool.Put(buf)

	buf.WriteString("UPDATE ")
	buf.WriteString(table)
	buf.WriteString(" SET ")
	firstField := true
	pairs := util.NewFieldPairs(values)
	args := make([]interface{}, 0, len(values)+1)
	for _, p := range pairs {
		if firstField {
			firstField = false
		} else {
			buf.WriteString(", ")
		}

		buf.WriteString(p.Field)
		buf.WriteString(`= ?`)
		args = appendArgs(args, p.Value)
	}
	buf.WriteString(" WHERE YCSB_KEY = ?")

	args = append(args, key)

	return db.execQuery(ctx, buf.String(), args...)
}

func appendArgs(args []interface{}, value []byte) []interface{} {
	if string(value) == "true" {
		args = append(args, true)
	} else if string(value) == "false" {
		args = append(args, false)
	} else {
		args = append(args, value)
	}
	return args
}

func (db *mysqlDB) Insert(ctx context.Context, table string, key string, values map[string][]byte) error {
	args := make([]interface{}, 0, 1+len(values))
	args = append(args, key)

	buf := db.bufPool.Get()
	defer db.bufPool.Put(buf)

	buf.WriteString("INSERT IGNORE INTO ")
	buf.WriteString(table)
	buf.WriteString(" (YCSB_KEY")

	pairs := util.NewFieldPairs(values)
	for _, p := range pairs {
		args = appendArgs(args, p.Value)
		buf.WriteString(" ,")
		buf.WriteString(p.Field)
	}
	buf.WriteString(") VALUES (?")

	for i := 0; i < len(pairs); i++ {
		buf.WriteString(" ,?")
	}

	buf.WriteByte(')')

	return db.execQuery(ctx, buf.String(), args...)
}

func (db *mysqlDB) Delete(ctx context.Context, table string, key string) error {
	query := fmt.Sprintf(`DELETE FROM %s WHERE YCSB_KEY = ?`, table)

	return db.execQuery(ctx, query, key)
}

func (db *mysqlDB) Analyze(ctx context.Context, table string) error {
	myDB, err := db.getDB()
	if err != nil {
		return err
	}
	_, err = myDB.Exec(fmt.Sprintf(`ANALYZE TABLE %s`, table))
	return err
}

func init() {
	ycsb.RegisterDBCreator("mysql", mysqlCreator{})
	ycsb.RegisterDBCreator("tidb", mysqlCreator{})
	ycsb.RegisterDBCreator("mariadb", mysqlCreator{})
}
