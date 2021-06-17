package keyshareserver

import (
	"encoding/binary"
	"html/template"
	"io/ioutil"
	"strings"

	irma "github.com/privacybydesign/irmago"
	"github.com/privacybydesign/irmago/internal/common"
	"github.com/privacybydesign/irmago/internal/keysharecore"
	"github.com/privacybydesign/irmago/server/keyshare"

	"github.com/dgrijalva/jwt-go"
	"github.com/go-errors/errors"
	"github.com/privacybydesign/irmago/server"
)

type DBType string

var errUnknownDBType = errors.New("Unknown database type")

const (
	DBTypeMemory   DBType = "memory"
	DBTypePostgres DBType = "postgres"
)

// Configuration contains configuration for the irmaserver library and irmad.
type Configuration struct {
	// IRMA server configuration
	*server.Configuration `mapstructure:",squash"`

	// Database configuration (ignored when database is provided)
	DBType    DBType `json:"db_type" mapstructure:"db_type"`
	DBConnStr string `json:"db_str" mapstructure:"db_str"`
	// Provide a prepared database (useful for testing)
	DB DB `json:"-"`

	// Configuration of secure Core
	// Private key used to sign JWTs with
	JwtKeyID          uint32 `json:"jwt_key_id" mapstructure:"jwt_key_id"`
	JwtIssuer         string `json:"jwt_issuer" mapstructure:"jwt_issuer"`
	JwtPinExpiry      int    `json:"jwt_pin_expiry" mapstructure:"jwt_pin_expiry"`
	JwtPrivateKey     string `json:"jwt_privkey" mapstructure:"jwt_privkey"`
	JwtPrivateKeyFile string `json:"jwt_privkey_file" mapstructure:"jwt_privkey_file"`
	// Decryption keys used for user secrets
	StorageFallbackKeyFiles []string `json:"storage_fallback_key_files" mapstructure:"storage_fallback_key_files"`
	StoragePrimaryKeyFile   string   `json:"storage_primary_key_file" mapstructure:"storage_primary_key_file"`

	// Keyshare attribute to issue during registration
	KeyshareAttribute irma.AttributeTypeIdentifier `json:"keyshare_attribute" mapstructure:"keyshare_attribute"`

	// Configuration for email sending during registration (email address use will be disabled if not present)
	keyshare.EmailConfiguration `mapstructure:",squash"`

	RegistrationEmailFiles     map[string]string `json:"registration_email_files" mapstructure:"registration_email_files"`
	RegistrationEmailSubjects  map[string]string `json:"registration_email_subjects" mapstructure:"registration_email_subjects"`
	registrationEmailTemplates map[string]*template.Template

	VerificationURL map[string]string `json:"verification_url" mapstructure:"verification_url"`
}

func readAESKey(filename string) (uint32, keysharecore.AESKey, error) {
	keyData, err := ioutil.ReadFile(filename)
	if err != nil {
		return 0, keysharecore.AESKey{}, err
	}
	if len(keyData) != 32+4 {
		return 0, keysharecore.AESKey{}, errors.New("Invalid aes key")
	}
	var key [32]byte
	copy(key[:], keyData[4:36])
	return binary.LittleEndian.Uint32(keyData[0:4]), key, nil
}

// Process a passed configuration to ensure all field values are valid and initialized
// as required by the rest of this keyshare server component.
func processConfiguration(conf *Configuration) (*keysharecore.Core, error) {
	// Setup email templates
	var err error
	if conf.EmailServer != "" {
		conf.registrationEmailTemplates, err = keyshare.ParseEmailTemplates(
			conf.RegistrationEmailFiles,
			conf.RegistrationEmailSubjects,
			conf.DefaultLanguage,
		)
		if err != nil {
			return nil, server.LogError(err)
		}
		if _, ok := conf.VerificationURL[conf.DefaultLanguage]; !ok {
			return nil, server.LogError(errors.Errorf("Missing verification base url for default language"))
		}
	}

	if err = conf.VerifyEmailServer(); err != nil {
		return nil, server.LogError(err)
	}

	if conf.IrmaConfiguration.AttributeTypes[conf.KeyshareAttribute] == nil {
		return nil, server.LogError(errors.Errorf("Unknown keyshare attribute: %s", conf.KeyshareAttribute))
	}
	_, err = conf.IrmaConfiguration.PrivateKeys.Latest(conf.KeyshareAttribute.CredentialTypeIdentifier().IssuerIdentifier())
	if err != nil {
		return nil, server.LogError(errors.Errorf("Failed to load private key of keyshare attribute: %v", err))
	}

	// Setup database
	if conf.DB == nil {
		switch conf.DBType {
		case DBTypeMemory:
			conf.DB = NewMemoryDB()
		case DBTypePostgres:
			var err error
			conf.DB, err = newPostgresDB(conf.DBConnStr)
			if err != nil {
				return nil, server.LogError(err)
			}
		default:
			return nil, server.LogError(errUnknownDBType)
		}
	}

	// Setup IRMA session server url for in QR code
	if !strings.HasSuffix(conf.URL, "/") {
		conf.URL += "/"
	}
	conf.URL += "irma/"

	// Parse keysharecore private keys and create a valid keyshare core
	if conf.JwtPrivateKey == "" && conf.JwtPrivateKeyFile == "" {
		return nil, server.LogError(errors.Errorf("Missing keyshare server jwt key"))
	}
	keybytes, err := common.ReadKey(conf.JwtPrivateKey, conf.JwtPrivateKeyFile)
	if err != nil {
		return nil, server.LogError(errors.WrapPrefix(err, "failed to read keyshare server jwt key", 0))
	}
	jwtPrivateKey, err := jwt.ParseRSAPrivateKeyFromPEM(keybytes)
	if err != nil {
		return nil, server.LogError(errors.WrapPrefix(err, "failed to read keyshare server jwt key", 0))
	}
	encID, encKey, err := readAESKey(conf.StoragePrimaryKeyFile)
	if err != nil {
		return nil, server.LogError(errors.WrapPrefix(err, "failed to load primary storage key", 0))
	}

	core := keysharecore.NewKeyshareCore(&keysharecore.Configuration{
		DecryptionKeyID: encID,
		DecryptionKey:   encKey,
		JWTPrivateKeyID: conf.JwtKeyID,
		JWTPrivateKey:   jwtPrivateKey,
		JWTIssuer:       conf.JwtIssuer,
		JWTPinExpiry:    conf.JwtPinExpiry,
	})
	for _, keyFile := range conf.StorageFallbackKeyFiles {
		id, key, err := readAESKey(keyFile)
		if err != nil {
			return nil, server.LogError(errors.WrapPrefix(err, "failed to load fallback key "+keyFile, 0))
		}
		core.DangerousAddDecryptionKey(id, key)
	}

	return core, nil
}
