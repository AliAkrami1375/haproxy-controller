package store

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

const certColumns = `id, name, file_name, domains, subject, issuer, serial, fingerprint,
	not_before, not_after, cert_pem, key_pem, chain_pem, created_at, updated_at`

func scanCert(row interface{ Scan(...any) error }) (*Certificate, error) {
	var (
		c                Certificate
		created, updated sql.NullString
	)
	err := row.Scan(&c.ID, &c.Name, &c.FileName, &c.Domains, &c.Subject, &c.Issuer, &c.Serial,
		&c.Fingerprint, &c.NotBefore, &c.NotAfter, &c.CertPEM, &c.KeyPEM, &c.ChainPEM,
		&created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(updated)
	return &c, nil
}

// ListCertificates returns all certificates ordered by name.
func (s *Store) ListCertificates(ctx context.Context) ([]*Certificate, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c, "SELECT "+certColumns+" FROM certificates ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Certificate
	for rows.Next() {
		cert, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cert)
	}
	return out, rows.Err()
}

// GetCertificate loads one certificate.
func (s *Store) GetCertificate(ctx context.Context, id int64) (*Certificate, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	return scanCert(s.db.QueryRowContext(c, "SELECT "+certColumns+" FROM certificates WHERE id = ?", id))
}

// ParseCertificate validates a PEM key pair and extracts the metadata shown in
// the UI. The chain, when supplied, is appended to the leaf on deploy.
func ParseCertificate(certPEM, keyPEM, chainPEM string) (*Certificate, error) {
	certPEM = strings.TrimSpace(certPEM)
	keyPEM = strings.TrimSpace(keyPEM)
	chainPEM = strings.TrimSpace(chainPEM)

	if certPEM == "" || keyPEM == "" {
		return nil, errors.New("both a certificate and a private key are required")
	}

	// X509KeyPair proves the private key actually matches the certificate.
	if _, err := tls.X509KeyPair([]byte(certPEM+"\n"+chainPEM), []byte(keyPEM)); err != nil {
		return nil, fmt.Errorf("certificate and key do not form a valid pair: %w", err)
	}

	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("the certificate field must contain a PEM CERTIFICATE block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	if chainPEM != "" {
		if b, _ := pem.Decode([]byte(chainPEM)); b == nil {
			return nil, errors.New("the chain field must contain PEM CERTIFICATE blocks")
		}
	}

	sum := sha256.Sum256(leaf.Raw)
	names := leaf.DNSNames
	if len(names) == 0 && leaf.Subject.CommonName != "" {
		names = []string{leaf.Subject.CommonName}
	}

	return &Certificate{
		Domains:     strings.Join(names, ", "),
		Subject:     leaf.Subject.String(),
		Issuer:      leaf.Issuer.String(),
		Serial:      leaf.SerialNumber.String(),
		Fingerprint: hex.EncodeToString(sum[:]),
		NotBefore:   leaf.NotBefore.UTC().Format(time.RFC3339),
		NotAfter:    leaf.NotAfter.UTC().Format(time.RFC3339),
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
		ChainPEM:    chainPEM,
	}, nil
}

// SaveCertificate validates and stores a certificate. The on-disk file name is
// derived from the certificate name so deploys are deterministic.
func (s *Store) SaveCertificate(ctx context.Context, cert *Certificate) (int64, error) {
	if err := ValidateName("certificate", cert.Name); err != nil {
		return 0, err
	}
	parsed, err := ParseCertificate(cert.CertPEM, cert.KeyPEM, cert.ChainPEM)
	if err != nil {
		return 0, err
	}
	parsed.ID = cert.ID
	parsed.Name = cert.Name
	parsed.FileName = cert.Name + ".pem"

	c, cancel := ctxTimeout(ctx)
	defer cancel()
	args := []any{parsed.Name, parsed.FileName, parsed.Domains, parsed.Subject, parsed.Issuer,
		parsed.Serial, parsed.Fingerprint, parsed.NotBefore, parsed.NotAfter,
		parsed.CertPEM, parsed.KeyPEM, parsed.ChainPEM}

	if parsed.ID == 0 {
		res, err := s.db.ExecContext(c, `
			INSERT INTO certificates (name, file_name, domains, subject, issuer, serial, fingerprint,
				not_before, not_after, cert_pem, key_pem, chain_pem)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
		if err != nil {
			if isUniqueViolation(err) {
				return 0, fmt.Errorf("a certificate named %q already exists", parsed.Name)
			}
			return 0, err
		}
		return res.LastInsertId()
	}

	args = append(args, parsed.ID)
	_, err = s.db.ExecContext(c, `
		UPDATE certificates SET name=?, file_name=?, domains=?, subject=?, issuer=?, serial=?,
			fingerprint=?, not_before=?, not_after=?, cert_pem=?, key_pem=?, chain_pem=?,
			updated_at=datetime('now')
		WHERE id = ?`, args...)
	if err != nil && isUniqueViolation(err) {
		return 0, fmt.Errorf("a certificate named %q already exists", parsed.Name)
	}
	return parsed.ID, err
}

// DeleteCertificate removes a certificate from the database. The stale file is
// cleaned up on the next deploy.
func (s *Store) DeleteCertificate(ctx context.Context, id int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, "DELETE FROM certificates WHERE id = ?", id)
	return err
}

// ExpiresAt parses NotAfter for expiry calculations in the UI.
func (c *Certificate) ExpiresAt() time.Time {
	t, err := time.Parse(time.RFC3339, c.NotAfter)
	if err != nil {
		return time.Time{}
	}
	return t
}

// DaysRemaining returns whole days until expiry; negative when expired.
func (c *Certificate) DaysRemaining() int {
	exp := c.ExpiresAt()
	if exp.IsZero() {
		return 0
	}
	return int(time.Until(exp).Hours() / 24)
}

// IsExpired reports whether the certificate is past its validity window.
func (c *Certificate) IsExpired() bool {
	exp := c.ExpiresAt()
	return !exp.IsZero() && time.Now().After(exp)
}

// IsExpiringSoon reports whether the certificate expires within 30 days.
func (c *Certificate) IsExpiringSoon() bool {
	d := c.DaysRemaining()
	return !c.IsExpired() && d >= 0 && d <= 30
}
