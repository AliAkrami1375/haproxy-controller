package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrorCodes are the status codes HAProxy can serve through `errorfile`.
var ErrorCodes = []int{200, 400, 401, 403, 404, 405, 407, 408, 410, 413, 425, 429,
	500, 501, 502, 503, 504}

// ValidErrorCode reports whether HAProxy accepts an errorfile for this code.
func ValidErrorCode(code int) bool {
	for _, c := range ErrorCodes {
		if c == code {
			return true
		}
	}
	return false
}

// StatusText maps the supported codes to their reason phrase.
func StatusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 407:
		return "Proxy Authentication Required"
	case 408:
		return "Request Timeout"
	case 410:
		return "Gone"
	case 413:
		return "Payload Too Large"
	case 425:
		return "Too Early"
	case 429:
		return "Too Many Requests"
	case 500:
		return "Internal Server Error"
	case 501:
		return "Not Implemented"
	case 502:
		return "Bad Gateway"
	case 503:
		return "Service Unavailable"
	case 504:
		return "Gateway Timeout"
	}
	return "Error"
}

const errorPageColumns = `id, name, group_name, status_code, content_type, headers, body,
	enabled, created_at, updated_at`

func scanErrorPage(row interface{ Scan(...any) error }) (*ErrorPage, error) {
	var (
		p                ErrorPage
		created, updated sql.NullString
		enabled          int
	)
	err := row.Scan(&p.ID, &p.Name, &p.GroupName, &p.StatusCode, &p.ContentType,
		&p.Headers, &p.Body, &enabled, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.Enabled = enabled != 0
	p.CreatedAt = parseTime(created)
	p.UpdatedAt = parseTime(updated)
	return &p, nil
}

// ListErrorPages returns every error page ordered by group then status code.
func (s *Store) ListErrorPages(ctx context.Context) ([]*ErrorPage, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c,
		"SELECT "+errorPageColumns+" FROM error_pages ORDER BY group_name, status_code")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*ErrorPage
	for rows.Next() {
		p, err := scanErrorPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetErrorPage loads one error page.
func (s *Store) GetErrorPage(ctx context.Context, id int64) (*ErrorPage, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	return scanErrorPage(s.db.QueryRowContext(c, "SELECT "+errorPageColumns+" FROM error_pages WHERE id = ?", id))
}

// ErrorPageGroups returns the distinct group names, which become `http-errors`
// sections in the generated configuration.
func (s *Store) ErrorPageGroups(ctx context.Context) ([]string, error) {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(c,
		"SELECT DISTINCT group_name FROM error_pages WHERE enabled = 1 ORDER BY group_name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// SaveErrorPage inserts or updates an error page.
func (s *Store) SaveErrorPage(ctx context.Context, p *ErrorPage) (int64, error) {
	if err := ValidateName("error page", p.Name); err != nil {
		return 0, err
	}
	if err := ValidateName("error page group", p.GroupName); err != nil {
		return 0, err
	}
	if !ValidErrorCode(p.StatusCode) {
		return 0, fmt.Errorf("HAProxy cannot serve a custom page for status %d", p.StatusCode)
	}
	if strings.TrimSpace(p.ContentType) == "" {
		p.ContentType = "text/html; charset=utf-8"
	}
	if err := checkSingleLine("content type", p.ContentType); err != nil {
		return 0, err
	}
	for _, h := range SplitLines(p.Headers) {
		if !strings.Contains(h, ":") {
			return 0, fmt.Errorf("header %q must be in \"Name: value\" form", h)
		}
	}

	c, cancel := ctxTimeout(ctx)
	defer cancel()
	args := []any{p.Name, p.GroupName, p.StatusCode, p.ContentType, p.Headers, p.Body, boolToInt(p.Enabled)}

	if p.ID == 0 {
		res, err := s.db.ExecContext(c, `
			INSERT INTO error_pages (name, group_name, status_code, content_type, headers, body, enabled)
			VALUES (?,?,?,?,?,?,?)`, args...)
		if err != nil {
			if isUniqueViolation(err) {
				return 0, fmt.Errorf("an error page named %q already exists", p.Name)
			}
			return 0, err
		}
		return res.LastInsertId()
	}

	args = append(args, p.ID)
	_, err := s.db.ExecContext(c, `
		UPDATE error_pages SET name=?, group_name=?, status_code=?, content_type=?, headers=?,
			body=?, enabled=?, updated_at=datetime('now')
		WHERE id = ?`, args...)
	if err != nil && isUniqueViolation(err) {
		return 0, fmt.Errorf("an error page named %q already exists", p.Name)
	}
	return p.ID, err
}

// DeleteErrorPage removes an error page.
func (s *Store) DeleteErrorPage(ctx context.Context, id int64) error {
	c, cancel := ctxTimeout(ctx)
	defer cancel()
	_, err := s.db.ExecContext(c, "DELETE FROM error_pages WHERE id = ?", id)
	return err
}

// FileName is the deployed file name for this page, unique per group+code.
func (p *ErrorPage) FileName() string {
	return fmt.Sprintf("%s-%d.http", p.GroupName, p.StatusCode)
}

// RawHTTP renders the page as the raw HTTP response HAProxy serves verbatim.
// HAProxy requires CRLF line endings and a Content-Length that matches the
// body exactly, so the body is normalised to CRLF before measuring.
func (p *ErrorPage) RawHTTP() string {
	body := strings.ReplaceAll(p.Body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")

	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", p.StatusCode, StatusText(p.StatusCode))
	fmt.Fprintf(&b, "Cache-Control: no-cache\r\n")
	fmt.Fprintf(&b, "Connection: close\r\n")
	fmt.Fprintf(&b, "Content-Type: %s\r\n", p.ContentType)
	for _, h := range SplitLines(p.Headers) {
		// Header values are single-line by construction; guard anyway.
		fmt.Fprintf(&b, "%s\r\n", strings.ReplaceAll(h, "\r", ""))
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}
