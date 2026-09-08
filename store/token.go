package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Token is a bearer token bound to a user with an access level.
type Token struct {
	ID      int64
	Token   string
	UserID  int64
	Level   string // "read" or "write"
	Created time.Time
}

// hashToken derives the storage form of a bearer token: hex(SHA-256(token)).
// Tokens are only ever compared by hash, so the database never holds the
// credential itself. The plaintext value is returned by CreateToken exactly
// once and must be shown to the user then.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateToken inserts a bearer token for a user. The token is stored as its
// SHA-256 hash; the returned Token carries the plaintext for one-time display.
func (s *CentralStore) CreateToken(token string, userID int64, level string) (*Token, error) {
	if level != "read" && level != "write" {
		return nil, fmt.Errorf("invalid token level %q (read|write)", level)
	}
	now := time.Now().UTC().UnixMilli()
	row := &tokenRow{Token: hashToken(token), UserID: userID, Level: level, Created: now}
	if err := s.d.gdb.Create(row).Error; err != nil {
		return nil, err
	}
	return &Token{ID: row.ID, Token: token, UserID: userID, Level: level, Created: time.UnixMilli(now)}, nil
}

// LookupToken resolves a token string to its user and level. It returns
// ErrTokenNotFound when the token is unknown. The plaintext token is echoed
// back in the returned record for interface stability (the caller supplied it).
func (s *CentralStore) LookupToken(token string) (*Token, error) {
	var row tokenRow
	err := s.d.gdb.Where("token=?", hashToken(token)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, err
	}
	return &Token{ID: row.ID, Token: token, UserID: row.UserID, Level: row.Level, Created: time.UnixMilli(row.Created)}, nil
}

// ListTokens returns all tokens for a user. The stored value is a SHA-256 hash
// and is NOT returned; Token.Token is empty (the plaintext is only available
// from CreateToken at creation time).
func (s *CentralStore) ListTokens(userID int64) ([]*Token, error) {
	var rows []tokenRow
	if err := s.d.gdb.Where("user_id=?", userID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Token, 0, len(rows))
	for i := range rows {
		out = append(out, &Token{ID: rows[i].ID, Token: "", UserID: rows[i].UserID, Level: rows[i].Level, Created: time.UnixMilli(rows[i].Created)})
	}
	return out, nil
}

// DeleteToken removes a token by its secret value.
func (s *CentralStore) DeleteToken(token string) error {
	res := s.d.gdb.Where("token=?", hashToken(token)).Delete(&tokenRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrTokenNotFound
	}
	return nil
}
