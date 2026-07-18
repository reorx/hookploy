package store

import "database/sql"

// TokenRecord is one active token row.
type TokenRecord struct {
	Kind    string
	Subject string
}

// InsertToken stores a new token hash.
func (s *Store) InsertToken(kind, subject, hash string) error {
	_, err := s.db.Exec(
		"INSERT INTO tokens (kind, subject, hash, created_at) VALUES (?, ?, ?, ?)",
		kind, subject, hash, now())
	return err
}

// LookupToken finds an unrevoked token by hash. Returns nil when absent.
func (s *Store) LookupToken(hash string) (*TokenRecord, error) {
	var rec TokenRecord
	err := s.db.QueryRow(
		"SELECT kind, subject FROM tokens WHERE hash = ? AND revoked_at IS NULL",
		hash).Scan(&rec.Kind, &rec.Subject)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// RevokeTokens revokes all active tokens of a kind+subject, returning how many.
func (s *Store) RevokeTokens(kind, subject string) (int64, error) {
	res, err := s.db.Exec(
		"UPDATE tokens SET revoked_at = ? WHERE kind = ? AND subject = ? AND revoked_at IS NULL",
		now(), kind, subject)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RotateToken inserts the new token and revokes all previous ones atomically.
func (s *Store) RotateToken(kind, subject, newHash string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		"UPDATE tokens SET revoked_at = ? WHERE kind = ? AND subject = ? AND revoked_at IS NULL",
		now(), kind, subject); err != nil {
		return err
	}
	if _, err := tx.Exec(
		"INSERT INTO tokens (kind, subject, hash, created_at) VALUES (?, ?, ?, ?)",
		kind, subject, newHash, now()); err != nil {
		return err
	}
	return tx.Commit()
}
