package hap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ebdaa/haproxy-controller/internal/store"
)

// Deployer renders, validates and applies configuration, and rolls back when
// HAProxy refuses to come up on the new file.
type Deployer struct {
	Store     *store.Store
	Renderer  *Renderer
	Validator *Validator

	ConfigPath    string
	ErrorPagesDir string
	CertsDir      string
	BackupDir     string

	// Process drives HAProxy itself: systemctl on a host, or direct child
	// supervision in a container.
	Process ProcessManager
}

// DeployResult describes the outcome of an apply for display in the UI.
type DeployResult struct {
	VersionID    int64
	Content      string
	Warnings     []string
	CheckOutput  string
	ReloadOutput string
	RolledBack   bool
	Duration     time.Duration
}

// Preview renders and validates without touching the live configuration. It
// stages error pages and certificates into a scratch tree so that `haproxy -c`
// can resolve every referenced path.
func (d *Deployer) Preview(ctx context.Context) (*Result, string, error) {
	res, err := d.Renderer.Render(ctx)
	if err != nil {
		return nil, "", err
	}

	staged, cleanup, err := d.stageForCheck(ctx, res.Content)
	if err != nil {
		return res, "", err
	}
	defer cleanup()

	out, err := d.Validator.Check(ctx, staged, nil)
	return res, out, err
}

// stageForCheck copies the generated config into a temporary tree together
// with the error pages and certificates it references, rewriting the paths so
// validation never depends on, or disturbs, the live directories.
func (d *Deployer) stageForCheck(ctx context.Context, content string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "haproxy-stage-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { os.RemoveAll(dir) }

	errDir := filepath.Join(dir, "errors")
	certDir := filepath.Join(dir, "certs")
	for _, p := range []string{errDir, certDir} {
		if err := os.MkdirAll(p, 0o750); err != nil {
			cleanup()
			return "", func() {}, err
		}
	}
	if err := d.writeErrorPages(ctx, errDir); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := d.writeCertificates(ctx, certDir); err != nil {
		cleanup()
		return "", func() {}, err
	}

	staged := strings.ReplaceAll(content, d.ErrorPagesDir, errDir)
	staged = strings.ReplaceAll(staged, d.CertsDir, certDir)
	return staged, cleanup, nil
}

// Apply is the "Save & Apply" action: render, stage, validate, write, reload,
// and roll back if HAProxy does not survive the reload.
func (d *Deployer) Apply(ctx context.Context, actor, comment string) (*DeployResult, error) {
	start := time.Now()
	out := &DeployResult{}

	res, err := d.Renderer.Render(ctx)
	if err != nil {
		return nil, err
	}
	out.Content = res.Content
	out.Warnings = res.Warnings

	// 1. Validate against a staged copy first, so a bad config never reaches
	//    the live directories.
	staged, cleanup, err := d.stageForCheck(ctx, res.Content)
	if err != nil {
		return nil, fmt.Errorf("stage configuration: %w", err)
	}
	checkOut, checkErr := d.Validator.Check(ctx, staged, nil)
	cleanup()
	out.CheckOutput = checkOut

	if checkErr != nil {
		id, _ := d.Store.AddVersion(ctx, &store.ConfigVersion{
			Content: res.Content, Status: store.VersionFailed,
			Comment: comment, CreatedBy: actor, Error: checkOut,
		})
		out.VersionID = id
		return out, fmt.Errorf("configuration check failed: %w", checkErr)
	}

	// 2. Keep the current file so a failed reload can be undone.
	previous, hadPrevious := d.readCurrent()
	if hadPrevious {
		if err := d.backup(previous); err != nil {
			return out, fmt.Errorf("back up current configuration: %w", err)
		}
	}

	// 3. Publish supporting files, then the config itself.
	if err := d.writeErrorPages(ctx, d.ErrorPagesDir); err != nil {
		return out, fmt.Errorf("write error pages: %w", err)
	}
	if err := d.writeCertificates(ctx, d.CertsDir); err != nil {
		return out, fmt.Errorf("write certificates: %w", err)
	}
	if err := writeFileAtomic(d.ConfigPath, []byte(res.Content), 0o640); err != nil {
		return out, fmt.Errorf("write %s: %w", d.ConfigPath, err)
	}

	versionID, err := d.Store.AddVersion(ctx, &store.ConfigVersion{
		Content: res.Content, Status: store.VersionApplied,
		Comment: comment, CreatedBy: actor,
	})
	if err != nil {
		return out, fmt.Errorf("record version: %w", err)
	}
	out.VersionID = versionID

	// 4. Reload, and undo everything if HAProxy does not come back.
	reloadOut, reloadErr := d.Process.Reload(ctx)
	out.ReloadOutput = reloadOut

	if reloadErr == nil {
		// A reload can report success and still leave HAProxy dead, so
		// confirm the process is actually up before calling this a win.
		time.Sleep(500 * time.Millisecond)
		if statusOut, ok := d.Process.Status(ctx); !ok {
			reloadErr = fmt.Errorf("haproxy is not running after reload: %s", strings.TrimSpace(statusOut))
			out.ReloadOutput = strings.TrimSpace(reloadOut + "\n" + statusOut)
		}
	}

	if reloadErr != nil {
		out.RolledBack = true
		_ = d.Store.MarkVersion(ctx, versionID, store.VersionFailed, out.ReloadOutput)

		if hadPrevious {
			if err := writeFileAtomic(d.ConfigPath, previous, 0o640); err == nil {
				rbOut, rbErr := d.Process.Reload(ctx)
				if rbErr != nil {
					out.ReloadOutput += "\nrollback reload also failed: " + strings.TrimSpace(rbOut)
				}
			}
		}
		return out, fmt.Errorf("reload failed and the previous configuration was restored: %w", reloadErr)
	}

	keep := d.Store.GetSettingInt(ctx, store.SetAutoBackupKeep, 30)
	_ = d.Store.TrimVersions(ctx, keep)

	out.Duration = time.Since(start)
	return out, nil
}

// readCurrent returns the live configuration file, if there is one.
func (d *Deployer) readCurrent() ([]byte, bool) {
	data, err := os.ReadFile(d.ConfigPath)
	if err != nil {
		return nil, false
	}
	return data, true
}

// backup copies the current configuration into the backup directory, keeping
// the 20 most recent copies.
func (d *Deployer) backup(content []byte) error {
	if err := os.MkdirAll(d.BackupDir, 0o750); err != nil {
		return err
	}
	name := fmt.Sprintf("haproxy-%s.cfg", time.Now().UTC().Format("20060102-150405"))
	if err := os.WriteFile(filepath.Join(d.BackupDir, name), content, 0o640); err != nil {
		return err
	}
	return d.trimBackups(20)
}

func (d *Deployer) trimBackups(keep int) error {
	entries, err := os.ReadDir(d.BackupDir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".cfg") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= keep {
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names[keep:] {
		_ = os.Remove(filepath.Join(d.BackupDir, n))
	}
	return nil
}

// writeErrorPages materialises every enabled error page and removes files for
// pages that no longer exist.
func (d *Deployer) writeErrorPages(ctx context.Context, dir string) error {
	pages, err := d.Store.ListErrorPages(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	wanted := map[string]bool{}
	for _, p := range pages {
		if !p.Enabled {
			continue
		}
		name := p.FileName()
		wanted[name] = true
		if err := writeFileAtomic(filepath.Join(dir, name), []byte(p.RawHTTP()), 0o644); err != nil {
			return err
		}
	}
	return pruneDir(dir, ".http", wanted)
}

// writeCertificates materialises every certificate as a single PEM bundle in
// the order HAProxy expects: key, leaf, then chain.
func (d *Deployer) writeCertificates(ctx context.Context, dir string) error {
	certs, err := d.Store.ListCertificates(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	wanted := map[string]bool{}
	for _, c := range certs {
		wanted[c.FileName] = true
		var sb strings.Builder
		sb.WriteString(strings.TrimSpace(c.KeyPEM))
		sb.WriteString("\n")
		sb.WriteString(strings.TrimSpace(c.CertPEM))
		sb.WriteString("\n")
		if chain := strings.TrimSpace(c.ChainPEM); chain != "" {
			sb.WriteString(chain)
			sb.WriteString("\n")
		}
		// Private key material: owner-readable only.
		if err := writeFileAtomic(filepath.Join(dir, c.FileName), []byte(sb.String()), 0o600); err != nil {
			return err
		}
	}
	return pruneDir(dir, ".pem", wanted)
}

// pruneDir removes files with the given suffix that are not in the wanted set.
// It only ever touches files the controller itself generates.
func pruneDir(dir, suffix string, wanted map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) || wanted[e.Name()] {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	return nil
}

// writeFileAtomic writes via a temporary file in the same directory followed
// by a rename, so readers never observe a partial file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Reload applies the configuration already on disk without re-rendering.
func (d *Deployer) Reload(ctx context.Context) (string, error) {
	return d.Process.Reload(ctx)
}

// Restart restarts HAProxy, dropping existing connections.
func (d *Deployer) Restart(ctx context.Context) (string, error) {
	return d.Process.Restart(ctx)
}

// Status reports whether HAProxy is running.
func (d *Deployer) Status(ctx context.Context) (string, bool) {
	return d.Process.Status(ctx)
}

// Rollback restores a stored version, validates it, writes it and reloads.
func (d *Deployer) Rollback(ctx context.Context, versionID int64, actor string) (*DeployResult, error) {
	v, err := d.Store.GetVersion(ctx, versionID)
	if err != nil {
		return nil, err
	}

	out := &DeployResult{Content: v.Content}
	staged, cleanup, err := d.stageForCheck(ctx, v.Content)
	if err != nil {
		return nil, err
	}
	checkOut, checkErr := d.Validator.Check(ctx, staged, nil)
	cleanup()
	out.CheckOutput = checkOut
	if checkErr != nil {
		return out, fmt.Errorf("the stored configuration no longer validates: %w", checkErr)
	}

	if current, ok := d.readCurrent(); ok {
		_ = d.backup(current)
	}
	if err := writeFileAtomic(d.ConfigPath, []byte(v.Content), 0o640); err != nil {
		return out, err
	}

	reloadOut, reloadErr := d.Process.Reload(ctx)
	out.ReloadOutput = reloadOut
	if reloadErr != nil {
		return out, fmt.Errorf("reload after rollback failed: %w", reloadErr)
	}

	newID, _ := d.Store.AddVersion(ctx, &store.ConfigVersion{
		Content:   v.Content,
		Status:    store.VersionApplied,
		Comment:   fmt.Sprintf("rollback to version %d", versionID),
		CreatedBy: actor,
	})
	out.VersionID = newID
	_ = d.Store.MarkVersion(ctx, versionID, store.VersionRolledBack, "")
	return out, nil
}
