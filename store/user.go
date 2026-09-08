package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// User is a Lab account. A user owns namespaces (orgs) and repositories.
type User struct {
	ID          int64
	Username    string
	DisplayName string
	Created     time.Time
}

// ErrUsernameTaken is returned when creating a user that already exists.
var ErrUsernameTaken = errors.New("store: username already exists")

// ErrTokenNotFound is returned when a token is unknown.
var ErrTokenNotFound = errors.New("store: token not found")

// CreateUser inserts a new user. Creating the first user closes the instance
// (anonymous access stops being allowed).
func (s *CentralStore) CreateUser(username, displayName string) (*User, error) {
	now := time.Now().UTC().UnixMilli()
	row := &userRow{Username: username, DisplayName: displayName, Created: now}
	if err := s.d.gdb.Create(row).Error; err != nil {
		return nil, err
	}
	s.invalidateOpenCache()
	return &User{ID: row.ID, Username: username, DisplayName: displayName, Created: time.UnixMilli(now)}, nil
}

// GetUser returns a user by id.
func (s *CentralStore) GetUser(id int64) (*User, error) {
	var row userRow
	err := s.d.gdb.Where("id=?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &User{ID: row.ID, Username: row.Username, DisplayName: row.DisplayName, Created: time.UnixMilli(row.Created)}, nil
}

// GetUserByUsername returns a user by username.
func (s *CentralStore) GetUserByUsername(username string) (*User, error) {
	var row userRow
	err := s.d.gdb.Where("username=?", username).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &User{ID: row.ID, Username: row.Username, DisplayName: row.DisplayName, Created: time.UnixMilli(row.Created)}, nil
}

// ListUsers returns all users ordered by username.
func (s *CentralStore) ListUsers() ([]*User, error) {
	var rows []userRow
	if err := s.d.gdb.Order("username").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*User, 0, len(rows))
	for i := range rows {
		out = append(out, &User{ID: rows[i].ID, Username: rows[i].Username, DisplayName: rows[i].DisplayName, Created: time.UnixMilli(rows[i].Created)})
	}
	return out, nil
}
