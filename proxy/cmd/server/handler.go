package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

type session struct {
	pool  *pool
	addr  string
	debug bool

	db     string       
	conn   *client.Conn 
	stmts  int         
	sticky bool        
}

func (s *session) acquire() (*client.Conn, error) {
	if s.conn != nil {
		return s.conn, nil
	}
	c, err := s.pool.get(context.Background())
	if err != nil {
		return nil, err
	}
	if s.db != "" && c.GetDB() != s.db {
		if err := c.UseDB(s.db); err != nil {
			s.pool.put(c, isMySQLError(err))
			return nil, err
		}
	}
	s.conn = c
	return c, nil
}


func (s *session) release(err error) {
	if s.conn == nil {
		return
	}
	if err != nil && !isMySQLError(err) {
		s.pool.put(s.conn, false)
		s.conn = nil
		return
	}
	if s.sticky || s.stmts > 0 || s.conn.IsInTransaction() {
		return
	}
	s.pool.put(s.conn, true)
	s.conn = nil
}

// close is called when the client disconnects.
func (s *session) close() {
	if s.conn == nil {
		return
	}
	
	s.pool.put(s.conn, false)
	s.conn = nil
}


func isMySQLError(err error) bool {
	var me *mysql.MyError
	return errors.As(err, &me)
}

func (s *session) UseDB(dbName string) error {
	c, err := s.acquire()
	if err != nil {
		return err
	}
	err = c.UseDB(dbName)
	if err == nil {
		s.db = dbName
	}
	s.release(err)
	return err
}

func (s *session) HandleQuery(query string) (*mysql.Result, error) {
	if s.debug {
		log.Printf("%s: query: %s", s.addr, query)
	}
	c, err := s.acquire()
	if err != nil {
		return nil, err
	}

	if db, ok := parseUse(query); ok {
		err = c.UseDB(db)
		if err == nil {
			s.db = db
		}
		s.release(err)
		return nil, err
	}

	if changesSessionState(query) {
		s.sticky = true
	}
	res, err := c.Execute(query)
	s.release(err)
	return res, err
}

func (s *session) HandleFieldList(table string, fieldWildcard string) ([]*mysql.Field, error) {
	c, err := s.acquire()
	if err != nil {
		return nil, err
	}
	fields, err := c.FieldList(table, fieldWildcard)
	s.release(err)
	return fields, err
}

func (s *session) HandleStmtPrepare(query string) (int, int, any, error) {
	c, err := s.acquire()
	if err != nil {
		return 0, 0, nil, err
	}
	stmt, err := c.Prepare(query)
	if err == nil {
		s.stmts++ // pins the connection until the statement is closed
	}
	s.release(err)
	if err != nil {
		return 0, 0, nil, err
	}
	return stmt.ParamNum(), stmt.ColumnNum(), stmt, nil
}

func (s *session) HandleStmtExecute(ctx any, query string, args []any) (*mysql.Result, error) {
	// The statement is pinned to s.conn, so no acquire needed.
	res, err := ctx.(*client.Stmt).Execute(args...)
	s.release(err)
	return res, err
}

func (s *session) HandleStmtClose(ctx any) error {
	err := ctx.(*client.Stmt).Close()
	s.stmts--
	s.release(err)
	return err
}

func (s *session) HandleOtherCommand(cmd byte, data []byte) error {
	return mysql.NewError(mysql.ER_UNKNOWN_ERROR, fmt.Sprintf("command %d not supported by proxy", cmd))
}

// parseUse recognises "USE dbname" sent as a plain query.
func parseUse(query string) (string, bool) {
	q := strings.TrimSpace(query)
	if len(q) < 4 || !strings.EqualFold(q[:4], "use ") {
		return "", false
	}
	return strings.Trim(strings.TrimSpace(q[4:]), "`;"), true
}

// changesSessionState is a deliberately simple check for statements whose
// effect outlives the statement on that connection. Being wrong in the
// "true" direction only costs pooling efficiency, never correctness.
func changesSessionState(query string) bool {
	q := strings.ToUpper(strings.TrimSpace(query))
	for _, prefix := range []string{"SET ", "LOCK ", "CREATE TEMPORARY", "PREPARE "} {
		if strings.HasPrefix(q, prefix) {
			return true
		}
	}
	return strings.Contains(q, "GET_LOCK(") || strings.Contains(q, "SQL_CALC_FOUND_ROWS")
}
