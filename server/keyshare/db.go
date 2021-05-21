package keyshare

import (
	"database/sql"

	"github.com/go-errors/errors"
	"github.com/privacybydesign/irmago/internal/common"
)

var ErrUserNotFound = errors.New("Could not find specified user")

type Tx interface {
	Commit() error
	Rollback() error
}

type DB struct {
	*sql.DB
}

func (db *DB) ExecCount(tx *sql.Tx, query string, args ...interface{}) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if tx != nil {
		res, err = tx.Exec(query, args...)
	} else {
		res, err = db.Exec(query, args...)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (db *DB) ExecUser(tx *sql.Tx, query string, args ...interface{}) error {
	c, err := db.ExecCount(tx, query, args...)
	if err != nil {
		return err
	}
	if c != 1 {
		return ErrUserNotFound
	}
	return nil
}

func (db *DB) QueryScan(tx *sql.Tx, query string, results []interface{}, args ...interface{}) error {
	var (
		res *sql.Rows
		err error
	)
	if tx != nil {
		res, err = tx.Query(query, args...)
	} else {
		res, err = db.Query(query, args...)
	}
	if err != nil {
		return err
	}
	defer common.Close(res)
	if !res.Next() {
		if err = res.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if results == nil {
		return nil
	}
	err = res.Scan(results...)
	if err != nil {
		return err
	}
	return nil
}

func (db *DB) QueryUser(tx *sql.Tx, query string, results []interface{}, args ...interface{}) error {
	err := db.QueryScan(tx, query, results, args...)
	if err == sql.ErrNoRows {
		return ErrUserNotFound
	}
	return err
}

func (db *DB) QueryIterate(tx *sql.Tx, query string, f func(rows *sql.Rows) error, args ...interface{}) error {
	var (
		res *sql.Rows
		err error
	)
	if tx != nil {
		res, err = tx.Query(query, args...)
	} else {
		res, err = db.Query(query, args...)
	}
	if err != nil {
		return err
	}
	defer common.Close(res)

	for res.Next() {
		if err = f(res); err != nil {
			return err
		}
	}
	return res.Err()
}
