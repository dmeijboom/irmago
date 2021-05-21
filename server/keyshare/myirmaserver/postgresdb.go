package myirmaserver

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/go-errors/errors"
	_ "github.com/jackc/pgx/stdlib"
	"github.com/privacybydesign/irmago/server"
	"github.com/privacybydesign/irmago/server/keyshare"
)

type myirmaPostgresDB struct {
	db keyshare.DB
}

const EMAIL_TOKEN_VALIDITY = 60 // amount of time an email login token is valid (in minutes)

var (
	ErrEmailNotFound = errors.New("Email address not found")
	ErrTokenNotFound = errors.New("Token not found")
)

func NewPostgresDatabase(connstring string) (MyirmaDB, error) {
	db, err := sql.Open("pgx", connstring)
	if err != nil {
		return nil, err
	}
	if err = db.Ping(); err != nil {
		return nil, errors.Errorf("failed to connect to database: %v", err)
	}
	return &myirmaPostgresDB{
		db: keyshare.DB{DB: db},
	}, nil
}

func (db *myirmaPostgresDB) UserID(username string) (int64, error) {
	var id int64
	return id, db.db.QueryUser(nil, "SELECT id FROM irma.users WHERE username = $1", []interface{}{&id}, username)
}

func (db *myirmaPostgresDB) VerifyEmailToken(tx keyshare.Tx, token string) (int64, error) {
	var email string
	var id int64
	err := db.db.QueryScan(tx.(*sql.Tx),
		"SELECT user_id, email FROM irma.email_verification_tokens WHERE token = $1 AND expiry >= $2",
		[]interface{}{&id, &email},
		token, time.Now().Unix())
	if err == sql.ErrNoRows {
		return 0, ErrTokenNotFound
	}
	if err != nil {
		return 0, err
	}

	err = db.AddEmail(id, email)
	if err != nil {
		return 0, err
	}

	// Beyond this point, errors are no longer relevant for frontend, so only log
	aff, err := db.db.ExecCount(tx.(*sql.Tx), "DELETE FROM irma.email_verification_tokens WHERE token = $1", token)
	if err != nil {
		_ = server.LogError(err)
		return id, nil
	}
	if aff != 1 {
		_ = server.LogError(errors.Errorf("Unexpected number of deleted records %d for token", aff))
		return id, nil
	}
	return id, nil
}

func (db *myirmaPostgresDB) RemoveUser(id int64, delay time.Duration) error {
	return db.db.ExecUser(nil, "UPDATE irma.users SET coredata = NULL, delete_on = $2 WHERE id = $1 AND coredata IS NOT NULL",
		id,
		time.Now().Add(delay).Unix())
}

func (db *myirmaPostgresDB) AddEmailLoginToken(tx keyshare.Tx, email, token string) (err error) {
	// Check if email address exists in database
	err = db.db.QueryScan(tx.(*sql.Tx), "SELECT 1 FROM irma.emails WHERE email = $1 AND (delete_on >= $2 OR delete_on IS NULL) LIMIT 1",
		nil, email, time.Now().Unix())
	if err == sql.ErrNoRows {
		err = ErrEmailNotFound
		return
	}
	if err != nil {
		return
	}

	// insert and verify
	aff, err := db.db.ExecCount(tx.(*sql.Tx), "INSERT INTO irma.email_login_tokens (token, email, expiry) VALUES ($1, $2, $3)",
		token,
		email,
		time.Now().Add(EMAIL_TOKEN_VALIDITY*time.Minute).Unix())
	if err != nil {
		return
	}
	if aff != 1 {
		err = errors.Errorf("Unexpected number of affected rows %d on token insert", aff)
		return
	}

	return nil
}

func (db *myirmaPostgresDB) LoginTokenCandidates(token string) ([]LoginCandidate, error) {
	var candidates []LoginCandidate
	err := db.db.QueryIterate(nil,
		`SELECT username, last_seen FROM irma.users INNER JOIN irma.emails ON users.id = emails.user_id WHERE
		     (emails.delete_on >= $2 OR emails.delete_on is NULL) AND
		          emails.email = (SELECT email FROM irma.email_login_tokens WHERE token = $1 AND expiry >= $2);`,
		func(rows *sql.Rows) error {
			candidate := LoginCandidate{}
			err := rows.Scan(&candidate.Username, &candidate.LastActive)
			candidates = append(candidates, candidate)
			return err
		},
		token, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, keyshare.ErrUserNotFound
	}
	return candidates, nil
}

func (db *myirmaPostgresDB) TryUserLoginToken(tx keyshare.Tx, token, username string) (int64, error) {
	var id int64
	err := db.db.QueryUser(tx.(*sql.Tx),
		`SELECT users.id FROM irma.users INNER JOIN irma.emails ON users.id = emails.user_id WHERE
		     username = $1 AND (emails.delete_on >= $3 OR emails.delete_on IS NULL) AND
		     email = (SELECT email FROM irma.email_login_tokens WHERE token = $2 AND expiry >= $3)`,
		[]interface{}{&id}, username, token, time.Now().Unix())
	if err != nil {
		return 0, err
	}

	aff, err := db.db.ExecCount(tx.(*sql.Tx), "DELETE FROM irma.email_login_tokens WHERE token = $1", token)
	if err != nil {
		return 0, err
	}
	if aff != 1 {
		return 0, errors.Errorf("Unexpected number of affected rows %d for token removal", aff)
	}
	return id, nil
}

func (db *myirmaPostgresDB) UserInformation(id int64) (UserInformation, error) {
	var result UserInformation

	// fetch username
	err := db.db.QueryUser(nil, "SELECT username, language, (coredata IS NULL) AS delete_in_progress FROM irma.users WHERE id = $1",
		[]interface{}{&result.Username, &result.language, &result.DeleteInProgress},
		id)
	if err != nil {
		return UserInformation{}, err
	}

	// fetch email addresses
	err = db.db.QueryIterate(nil,
		"SELECT email, (delete_on IS NOT NULL) AS delete_in_progress FROM irma.emails WHERE user_id = $1 AND (delete_on >= $2 OR delete_on IS NULL)",
		func(rows *sql.Rows) error {
			var email UserEmail
			err = rows.Scan(&email.Email, &email.DeleteInProgress)
			result.Emails = append(result.Emails, email)
			return err
		},
		id, time.Now().Unix())
	if err != nil {
		return UserInformation{}, err
	}
	return result, nil
}

func (db *myirmaPostgresDB) Logs(id int64, offset, amount int) ([]LogEntry, error) {
	var result []LogEntry
	err := db.db.QueryIterate(nil,
		"SELECT time, event, param FROM irma.log_entry_records WHERE user_id = $1 ORDER BY time DESC OFFSET $2 LIMIT $3",
		func(rows *sql.Rows) error {
			var curEntry LogEntry
			err := rows.Scan(&curEntry.Timestamp, &curEntry.Event, &curEntry.Param)
			result = append(result, curEntry)
			return err
		},
		id, offset, amount)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (db *myirmaPostgresDB) AddEmail(id int64, email string) error {
	// Try to restore email in process of deletion
	aff, err := db.db.ExecCount(nil, "UPDATE irma.emails SET delete_on = NULL WHERE user_id = $1 AND email = $2", id, email)
	if err != nil {
		return err
	}
	if aff == 1 {
		return nil
	}

	// Fall back to adding new one
	_, err = db.db.ExecCount(nil, "INSERT INTO irma.emails (user_id, email) VALUES ($1, $2)", id, email)
	return err
}

func (db *myirmaPostgresDB) RemoveEmail(id int64, email string, delay time.Duration) error {
	aff, err := db.db.ExecCount(nil, "UPDATE irma.emails SET delete_on = $3 WHERE user_id = $1 AND email = $2 AND delete_on IS NULL",
		id,
		email,
		time.Now().Add(delay).Unix())
	if err != nil {
		return err
	}
	if aff != 1 {
		return errors.Errorf("Unexpected number of affected rows %d for email removal", aff)
	}
	return nil
}

func (db *myirmaPostgresDB) SetSeen(tx keyshare.Tx, id int64) error {
	return db.db.ExecUser(tx.(*sql.Tx), "UPDATE irma.users SET last_seen = $1 WHERE id = $2", time.Now().Unix(), id)
}

func (db *myirmaPostgresDB) Tx(
	w http.ResponseWriter, r *http.Request,
	f func(tx keyshare.Tx) (server.Error, string),
) {
	tx, err := db.db.BeginTx(r.Context(), nil)
	if err != nil {
		server.Logger.WithField("error", err).Error("Error starting DB transaction")
		server.WriteError(w, server.ErrorInternal, err.Error())
		return
	}
	if e, msg := f(tx); e != (server.Error{}) {
		_ = tx.Rollback()
		server.WriteError(w, e, msg)
		return
	}

	// TODO: modify interface: if f was succesful, then the header is already written here
	if err = tx.Commit(); err != nil {
		server.Logger.WithField("error", err).Error("Error committing DB transaction")
		server.WriteError(w, server.ErrorInternal, err.Error())
	}
}
