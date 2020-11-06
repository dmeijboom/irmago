package keyshareserver

import (
	"database/sql"
	"encoding/json"
	"time"

	_ "github.com/jackc/pgx/stdlib"
	"github.com/privacybydesign/irmago/internal/common"
)

// postgresDB provides a postgres-backed implementation of KeyshareDB
// database access is done through the database/sql mechanisms, using
// pgx as database driver

type keysharePostgresDatabase struct {
	db *sql.DB
}

// For easy access in the database, we store the row id also in the returned user data
type keysharePostgresUser struct {
	KeyshareUserData
	id int64
}

func (m *keysharePostgresUser) Data() *KeyshareUserData {
	return &m.KeyshareUserData
}

const MAX_PIN_TRIES = 3         // Number of tries allowed on pin before we start with exponential backoff
const BACKOFF_START = 30        // Initial ammount of time you are forced to back off when having multiple pin failures (in seconds)
const EMAIL_TOKEN_VALIDITY = 24 // Ammount of time your email validation token is valid (in hours)

func NewPostgresDatabase(connstring string) (KeyshareDB, error) {
	db, err := sql.Open("pgx", connstring)
	if err != nil {
		return nil, err
	}
	return &keysharePostgresDatabase{
		db: db,
	}, nil
}

func (db *keysharePostgresDatabase) NewUser(user KeyshareUserData) (KeyshareUser, error) {
	res, err := db.db.Query("INSERT INTO irma.users (username, language, coredata, last_seen, pin_counter, pin_block_date) VALUES ($1, $2, $3, $4, 0, 0) RETURNING id",
		user.Username,
		user.Language,
		user.Coredata[:],
		time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer common.Close(res)
	if !res.Next() {
		return nil, ErrUserAlreadyExists
	}
	var id int64
	err = res.Scan(&id)
	if err != nil {
		return nil, err
	}
	return &keysharePostgresUser{KeyshareUserData: user, id: id}, nil
}

func (db *keysharePostgresDatabase) User(username string) (KeyshareUser, error) {
	rows, err := db.db.Query("SELECT id, username, language, coredata FROM irma.users WHERE username = $1 AND coredata IS NOT NULL", username)
	if err != nil {
		return nil, err
	}
	defer common.Close(rows)
	if !rows.Next() {
		return nil, ErrUserNotFound
	}
	var result keysharePostgresUser
	var ep []byte
	err = rows.Scan(&result.id, &result.Username, &result.Language, &ep)
	if err != nil {
		return nil, err
	}
	if len(ep) != len(result.Coredata[:]) {
		return nil, ErrInvalidRecord
	}
	copy(result.Coredata[:], ep)
	return &result, nil
}

func (db *keysharePostgresDatabase) UpdateUser(user KeyshareUser) error {
	userdata := user.(*keysharePostgresUser)
	res, err := db.db.Exec("UPDATE irma.users SET username=$1, language=$2, coredata=$3 WHERE id=$4",
		userdata.Username,
		userdata.Language,
		userdata.Coredata[:],
		userdata.id)
	if err != nil {
		return err
	}
	c, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if c == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (db *keysharePostgresDatabase) ReservePincheck(user KeyshareUser) (bool, int, int64, error) {
	// Extract data
	userdata := user.(*keysharePostgresUser)

	// Check that account is not blocked already, and if not,
	//  update pinCounter and pinBlockDate
	uprows, err := db.db.Query(`
		UPDATE irma.users
		SET pin_counter = pin_counter+1,
			pin_block_date = $1+$2*2^GREATEST(0, pin_counter-$3)
		WHERE id=$4 AND pin_block_date<=$5 AND coredata IS NOT NULL
		RETURNING pin_counter, pin_block_date`,
		time.Now().Unix()-1-BACKOFF_START, // Grace time of 2 seconds on pinBlockDate set
		BACKOFF_START,
		MAX_PIN_TRIES-2,
		userdata.id,
		time.Now().Unix())
	if err != nil {
		return false, 0, 0, err
	}
	defer common.Close(uprows)

	// Check whether we have results
	if !uprows.Next() {
		// if no, then account either does not exist (which would be weird here) or is blocked
		// so request wait timeout
		pinrows, err := db.db.Query("SELECT pin_block_date FROM irma.users WHERE id=$1 AND coredata IS NOT NULL", userdata.id)
		if err != nil {
			return false, 0, 0, err
		}
		defer common.Close(pinrows)
		if !pinrows.Next() {
			return false, 0, 0, ErrUserNotFound
		}
		var wait int64
		err = pinrows.Scan(&wait)
		if err != nil {
			return false, 0, 0, err
		}
		wait = wait - time.Now().Unix()
		if wait < 0 {
			wait = 0
		}
		return false, 0, wait, nil
	}

	// Pin check is allowed (implied since there is a result, so pinBlockDate <= now)
	//  calculate tries remaining and wait time
	var tries int
	var wait int64
	err = uprows.Scan(&tries, &wait)
	if err != nil {
		return false, 0, 0, err
	}
	tries = MAX_PIN_TRIES - tries
	if tries < 0 {
		tries = 0
	}
	wait = wait - time.Now().Unix()
	if wait < 0 {
		wait = 0
	}
	return true, tries, wait, nil
}

func (db *keysharePostgresDatabase) ClearPincheck(user KeyshareUser) error {
	userdata := user.(*keysharePostgresUser)
	res, err := db.db.Exec("UPDATE irma.users SET pin_counter=0, pin_block_date=0 WHERE id=$1", userdata.id)
	if err != nil {
		return err
	}
	c, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if c == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (db *keysharePostgresDatabase) SetSeen(user KeyshareUser) error {
	userdata := user.(*keysharePostgresUser)
	res, err := db.db.Exec("UPDATE irma.users SET last_seen = $1 WHERE id = $2", time.Now().Unix(), userdata.id)
	if err != nil {
		return err
	}
	c, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if c == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (db *keysharePostgresDatabase) AddLog(user KeyshareUser, eventType LogEntryType, param interface{}) error {
	userdata := user.(*keysharePostgresUser)

	var encodedParamString *string
	if param != nil {
		encodedParam, err := json.Marshal(param)
		if err != nil {
			return err
		}
		encodedParams := string(encodedParam)
		encodedParamString = &encodedParams
	}

	_, err := db.db.Exec("INSERT INTO irma.log_entry_records (time, event, param, user_id) VALUES ($1, $2, $3, $4)",
		time.Now().Unix(),
		eventType,
		encodedParamString,
		userdata.id)
	return err
}

func (db *keysharePostgresDatabase) AddEmailVerification(user KeyshareUser, emailAddress, token string) error {
	userdata := user.(*keysharePostgresUser)

	_, err := db.db.Exec("INSERT INTO irma.email_verification_tokens (token, email, user_id, expiry) VALUES ($1, $2, $3, $4)",
		token,
		emailAddress,
		userdata.id,
		time.Now().Add(EMAIL_TOKEN_VALIDITY*time.Hour).Unix())
	return err
}
