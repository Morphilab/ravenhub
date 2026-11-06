// Package store holds every SQL query of RavenHub. Postgres is the single
// datastore: config, queue, deliveries and audit.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ravenhub/internal/crypto"
	"ravenhub/internal/model"
)

// ErrNotFound is returned by single-row lookups that miss.
var ErrNotFound = errors.New("no encontrado")

// ErrConflict is returned by uniqueness violations (duplicate names, etc.).
var ErrConflict = errors.New("ya existe")

// Store wraps the pool and the master key used to (de)crypt account creds.
// LegacyMasterKey (nil = sin fallback) solo descifra filas legacy.
type Store struct {
	Pool            *pgxpool.Pool
	MasterKey       []byte
	MidMasterKey    []byte // KDF legacy (PBKDF2 210k): solo dual-read
	LegacyMasterKey []byte // derivación legacy (SHA256): solo dual-read
}

// Domain types ---------------------------------------------------------------

type Project struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Options   json.RawMessage `json:"options"`
	CreatedAt time.Time       `json:"created_at"`
}

type TokenRow struct {
	ID          string
	ProjectID   *string
	ProjectName string
	ProjectOpts json.RawMessage
	Name        string
	Hash        string
	Prefix      string
	Scopes      model.Scopes
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
}

func (t TokenRow) IsAdmin() bool   { return t.ProjectID == nil && t.Scopes.Admin }
func (t TokenRow) IsRevoked() bool { return t.RevokedAt != nil }

type Account struct {
	ID             string
	ProjectID      string
	Channel        string
	Label          string
	CredsEncrypted []byte
	Limits         json.RawMessage
}

type Destination struct {
	ID         string          `json:"id"`
	ProjectID  string          `json:"project_id"`
	Name       string          `json:"name"`
	AccountID  string          `json:"account_id"`
	Target     json.RawMessage `json:"target"`
	QuietHours json.RawMessage `json:"quiet_hours"`
}

// ResolvedDestination is what a worker needs to deliver one copy.
type ResolvedDestination struct {
	DestID     string
	DestName   string
	Channel    string
	Target     json.RawMessage
	QuietHours json.RawMessage
	AccountID  string
}

type MessageRow struct {
	ID              string
	ProjectID       string
	Payload         json.RawMessage
	Priority        model.Priority
	Status          string
	DedupKey        *string
	ExpiresAt       time.Time
	Attempts        int
	NextAttemptAt   time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	SendAt          *time.Time
	AggregationKey  *string
	AggregatedCount int
	IsDigest        bool
}

type DeliveryRow struct {
	ID            string
	DestName      string
	Channel       string
	Status        string
	Error         *string
	ProviderMsgID *string
	LatencyMs     *int
	Truncated     bool // el contenido se cortó por el límite del canal
	// joined info for pending deliveries (nil si el destino fue borrado)
	DestID     *string
	Target     json.RawMessage
	QuietHours json.RawMessage
	AccountID  *string
	CredsEnc   []byte
	AccountCh  *string
}

// Upsert helpers for the seed importer ---------------------------------------

// scopesFor builds normalized scopes (sorted, deduplicated destinations) and
// validates max_priority. La normalización hace los scopes comparables byte a
// byte entre re-imports del seed (que no impone orden).
func scopesFor(destinations []string, maxPriority string) (model.Scopes, error) {
	scopes := model.Scopes{Destinations: normalizeDestinations(destinations)}
	if maxPriority != "" {
		p, err := model.ParsePriority(maxPriority)
		if err != nil {
			return model.Scopes{}, fmt.Errorf("max_priority inválido %q: %w", maxPriority, err)
		}
		// valor normalizado (idem admin API): el crudo no debe dejar el techo
		// fail-open
		scopes.MaxPriority = p
	}
	return scopes, nil
}

// normalizeDestinations sorts and deduplicates a destination list.
func normalizeDestinations(d []string) []string {
	if len(d) == 0 {
		return nil
	}
	out := append([]string(nil), d...)
	sort.Strings(out)
	uniq := out[:0]
	for i, s := range out {
		if i == 0 || s != out[i-1] {
			uniq = append(uniq, s)
		}
	}
	return uniq
}

// EnsureProjectToken creates the named token if absent; returns the plaintext
// only when it was just created ("" otherwise).
func (s *Store) EnsureProjectToken(ctx context.Context, projectID, name string, destinations []string, maxPriority string) (string, error) {
	// semántica: un token REVOCADO con el mismo nombre cuenta como
	// inexistente — el re-import de un seed que declara ese token crea uno
	// nuevo (revocar es una acción deliberada y el seed declara estado
	// deseado); si sigue ACTIVO, no se regenera jamás.
	var existing string
	err := s.Pool.QueryRow(ctx,
		`SELECT prefix FROM tokens WHERE project_id=$1 AND name=$2 AND revoked_at IS NULL`, projectID, name).Scan(&existing)
	if err == nil {
		return "", nil // ya existe: no regenerar
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	scopes, err := scopesFor(destinations, maxPriority)
	if err != nil {
		return "", err
	}
	full, _, err := s.CreateToken(ctx, &projectID, name, scopes, nil)
	return full, err
}

// UpdateTokenScopes actualiza los scopes de un token ACTIVO por (proyecto,
// nombre) si difieren; el secreto no se regenera. Devuelve true si cambió.
// Declarativo: el seed es fuente de verdad también sobre scopes; un token
// existente no queda con los scopes viejos para siempre.
func (s *Store) UpdateTokenScopes(ctx context.Context, projectID, name string, destinations []string, maxPriority string) (bool, error) {
	scopes, err := scopesFor(destinations, maxPriority)
	if err != nil {
		return false, err
	}
	scopesJSON, _ := json.Marshal(scopes)
	tag, err := s.Pool.Exec(ctx, `
		UPDATE tokens SET scopes=$3::jsonb
		WHERE project_id=$1 AND name=$2 AND revoked_at IS NULL
		  AND scopes IS DISTINCT FROM $3::jsonb`, projectID, name, scopesJSON)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UpsertAccount inserts or updates an account by (project, label).
func (s *Store) UpsertAccount(ctx context.Context, projectID, channel, label string, credsPlain, limits []byte) error {
	enc, err := crypto.EncryptSecretV3(s.MasterKey, credsPlain)
	if err != nil {
		return err
	}
	if limits == nil {
		limits = []byte(`{}`)
	}
	_, err = s.Pool.Exec(ctx, `
		INSERT INTO accounts(project_id, channel, label, creds_encrypted, limits)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (project_id, label) DO UPDATE
		SET channel=$2, creds_encrypted=$4, limits=$5`,
		projectID, channel, label, enc, limits)
	return err
}

// GetAccountByLabel resolves an account inside a project namespace.
func (s *Store) GetAccountByLabel(ctx context.Context, projectID, label string) (Account, error) {
	var a Account
	err := s.Pool.QueryRow(ctx, `
		SELECT id, project_id, channel, label, creds_encrypted, limits
		FROM accounts WHERE project_id=$1 AND label=$2`, projectID, label,
	).Scan(&a.ID, &a.ProjectID, &a.Channel, &a.Label, &a.CredsEncrypted, &a.Limits)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

// UpsertDestination inserts or updates a destination by (project, name).
func (s *Store) UpsertDestination(ctx context.Context, projectID, name, accountID string, target, quietHours []byte) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO destinations(project_id, name, account_id, target, quiet_hours)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (project_id, name) DO UPDATE
		SET account_id=$3, target=$4, quiet_hours=$5`,
		projectID, name, accountID, target, quietHours)
	return err
}

// UpsertGroup replaces the membership of a named group.
func (s *Store) UpsertGroup(ctx context.Context, projectID, name string, destNames []string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var groupID string
	err = tx.QueryRow(ctx, `
		INSERT INTO destination_groups(project_id, name) VALUES ($1,$2)
		ON CONFLICT (project_id, name) DO UPDATE SET name=EXCLUDED.name
		RETURNING id`, projectID, name).Scan(&groupID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM destination_group_members WHERE group_id=$1`, groupID); err != nil {
		return err
	}
	for _, dn := range destNames {
		var destID string
		err := tx.QueryRow(ctx,
			`SELECT id FROM destinations WHERE project_id=$1 AND name=$2`, projectID, dn).Scan(&destID)
		if err != nil {
			return fmt.Errorf("destino %q: %w", dn, ErrNotFound)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO destination_group_members(group_id, destination_id) VALUES ($1,$2)`, groupID, destID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Projects -------------------------------------------------------------------

func (s *Store) CreateProject(ctx context.Context, name string, options json.RawMessage) (Project, error) {
	if options == nil {
		options = json.RawMessage(`{}`)
	}
	var p Project
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO projects(name, options) VALUES ($1, $2)
		RETURNING id, name, options, created_at`, name, options,
	).Scan(&p.ID, &p.Name, &p.Options, &p.CreatedAt)
	if err != nil {
		if isDuplicate(err) {
			return Project{}, fmt.Errorf("proyecto %q: %w", name, ErrConflict)
		}
		return Project{}, err
	}
	return p, nil
}

func (s *Store) GetProjectByName(ctx context.Context, name string) (Project, error) {
	var p Project
	err := s.Pool.QueryRow(ctx,
		`SELECT id, name, options, created_at FROM projects WHERE name=$1`, name,
	).Scan(&p.ID, &p.Name, &p.Options, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

// UpdateProjectOptions replaces a project's options (declarativo: seed y
// admin son autoritativos). El caller valida con
// model.ValidateProjectOptions. Devuelve changed=false si ya tenía esos
// options (idempotente).
func (s *Store) UpdateProjectOptions(ctx context.Context, id string, options json.RawMessage) (bool, error) {
	if options == nil {
		options = json.RawMessage(`{}`)
	}
	tag, err := s.Pool.Exec(ctx,
		`UPDATE projects SET options=$2::jsonb WHERE id=$1 AND options IS DISTINCT FROM $2::jsonb`,
		id, options)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1)`, id).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, ErrNotFound
	}
	return false, nil
}

// DeleteProject borra un proyecto y su grafo operativo vía FK CASCADE. Antes
// de borrarlo, anonimiza sus filas de auditoría (project_id/token_prefix/
// subject/detail) dentro de la misma transacción: el evento de borrado global
// queda en la API y la auditoría agregada sobrevive sin PII. El flag
// `ravenhub.privacy_purge` es transaction-local y solo permite UPDATE.
func (s *Store) DeleteProject(ctx context.Context, id string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`SELECT set_config('ravenhub.privacy_purge', 'on', true)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE audit_log
		   SET project_id = NULL,
		       token_prefix = '',
		       subject = '',
		       detail = '{}'::jsonb
		 WHERE project_id = $1`, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM projects WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, name, options, created_at FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Options, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Tokens ---------------------------------------------------------------------

// CreateToken generates a fresh API token, stores only its hash and returns
// the plaintext exactly once.
func (s *Store) CreateToken(ctx context.Context, projectID *string, name string, scopes model.Scopes, expiresAt *time.Time) (string, TokenRow, error) {
	var full, prefix string
	var err error
	if projectID == nil {
		full, prefix, err = crypto.NewAdminToken()
	} else {
		projectName := ""
		if *projectID != "" {
			var p Project
			p, err = s.projectByID(ctx, *projectID)
			if err != nil {
				return "", TokenRow{}, err
			}
			projectName = p.Name
		}
		full, prefix, err = crypto.NewToken(projectName)
	}
	if err != nil {
		return "", TokenRow{}, err
	}
	scopesJSON, _ := json.Marshal(scopes)
	row := TokenRow{Prefix: prefix}
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO tokens(project_id, name, hash, prefix, scopes, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, hash`, projectID, name, crypto.HashToken(full), prefix, scopesJSON, expiresAt,
	).Scan(&row.ID, &row.Hash)
	if err != nil {
		if isDuplicate(err) {
			return "", TokenRow{}, fmt.Errorf("token %q: %w", name, ErrConflict)
		}
		return "", TokenRow{}, err
	}
	row.ProjectID = projectID
	row.Name = name
	row.Scopes = scopes
	row.ExpiresAt = expiresAt
	return full, row, nil
}

func (s *Store) projectByID(ctx context.Context, id string) (Project, error) {
	var p Project
	err := s.Pool.QueryRow(ctx, `SELECT id, name, options, created_at FROM projects WHERE id=$1`, id).
		Scan(&p.ID, &p.Name, &p.Options, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

// EnsureAdminToken upserts the bootstrap admin token (by hash). Rotar el
// valor en .env debe invalidar los bootstrap anteriores: los demás tokens
// 'bootstrap-admin' con hash distinto quedan revocados, y si el mismo token
// vuelve a configurarse se re-activa.
func (s *Store) EnsureAdminToken(ctx context.Context, token string) error {
	hash := crypto.HashToken(token)
	if _, err := s.Pool.Exec(ctx, `
		UPDATE tokens SET revoked_at=now()
		WHERE project_id IS NULL AND name='bootstrap-admin' AND hash <> $1 AND revoked_at IS NULL`, hash); err != nil {
		return err
	}
	// El prefijo es visible en listados/auditoría: nunca 10 chars de un token
	// custom que no sea `rva_admin_…` (RH/I-03). Para admin, 4 chars bastan.
	prefix := "admin"
	if len(token) >= 14 && token[:10] == "rva_admin_" {
		prefix = token[:14]
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO tokens(project_id, name, hash, prefix, scopes)
		VALUES (NULL, 'bootstrap-admin', $1, $2, '{"admin":true}')
		ON CONFLICT (hash) DO UPDATE SET revoked_at=NULL, prefix=EXCLUDED.prefix`,
		hash, prefix)
	return err
}

// FindTokenByHash resolves a Bearer token to its row (auth path, uncached:
// revocar invalida al instante).
func (s *Store) FindTokenByHash(ctx context.Context, hash string) (TokenRow, error) {
	var t TokenRow
	err := s.Pool.QueryRow(ctx, `
		SELECT t.id, t.project_id, COALESCE(p.name,''), COALESCE(p.options,'{}'::jsonb),
		       t.name, t.hash, t.prefix, t.scopes, t.expires_at, t.revoked_at
		FROM tokens t LEFT JOIN projects p ON p.id = t.project_id
		WHERE t.hash = $1`, hash,
	).Scan(&t.ID, &t.ProjectID, &t.ProjectName, &t.ProjectOpts,
		&t.Name, &t.Hash, &t.Prefix, &t.Scopes, &t.ExpiresAt, &t.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenRow{}, ErrNotFound
	}
	return t, err
}

func (s *Store) RevokeToken(ctx context.Context, id string) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE tokens SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	return tag.RowsAffected() > 0, err
}

// RotateToken revokes the old token and mints a new one with the same scopes.
// El nuevo hereda la caducidad del viejo: rotar no extiende validez. La
// revocación va ANTES del INSERT dentro de la misma tx: el índice parcial
// único de tokens activos evalúa el INSERT contra los activos del momento,
// y con el viejo aún visible el gemelo chocaba SIEMPRE con unique violation.
func (s *Store) RotateToken(ctx context.Context, id string) (string, TokenRow, error) {
	old, err := s.tokenByID(ctx, id)
	if err != nil {
		return "", TokenRow{}, err
	}
	var expires *time.Time
	if old.ExpiresAt != nil {
		t := *old.ExpiresAt
		expires = &t
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", TokenRow{}, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE tokens SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	if err != nil {
		return "", TokenRow{}, err
	}
	if tag.RowsAffected() == 0 {
		return "", TokenRow{}, ErrNotFound // ya estaba revocado: rotarlo reviviría el nombre
	}
	var full, prefix string
	if old.ProjectID == nil {
		full, prefix, err = crypto.NewAdminToken()
	} else {
		full, prefix, err = crypto.NewToken(old.ProjectName)
	}
	if err != nil {
		return "", TokenRow{}, err
	}
	scopesJSON, _ := json.Marshal(old.Scopes)
	row := TokenRow{Prefix: prefix, ProjectID: old.ProjectID, Name: old.Name, Scopes: old.Scopes, ExpiresAt: expires}
	if err := tx.QueryRow(ctx, `
		INSERT INTO tokens(project_id, name, hash, prefix, scopes, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, hash`, old.ProjectID, old.Name, crypto.HashToken(full), prefix, scopesJSON, expires,
	).Scan(&row.ID, &row.Hash); err != nil {
		return "", TokenRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", TokenRow{}, err
	}
	return full, row, nil
}

func (s *Store) tokenByID(ctx context.Context, id string) (TokenRow, error) {
	var t TokenRow
	err := s.Pool.QueryRow(ctx, `
		SELECT t.id, t.project_id, COALESCE(p.name,''), COALESCE(p.options,'{}'::jsonb),
		       t.name, t.hash, t.prefix, t.scopes, t.expires_at, t.revoked_at
		FROM tokens t LEFT JOIN projects p ON p.id = t.project_id WHERE t.id=$1`, id,
	).Scan(&t.ID, &t.ProjectID, &t.ProjectName, &t.ProjectOpts,
		&t.Name, &t.Hash, &t.Prefix, &t.Scopes, &t.ExpiresAt, &t.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenRow{}, ErrNotFound
	}
	return t, err
}

func (s *Store) ListTokens(ctx context.Context, projectID string) ([]TokenRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT t.id, t.project_id, COALESCE(p.name,''), COALESCE(p.options,'{}'::jsonb),
		       t.name, t.hash, t.prefix, t.scopes, t.expires_at, t.revoked_at
		FROM tokens t LEFT JOIN projects p ON p.id = t.project_id
		WHERE t.project_id = $1 ORDER BY t.created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenRow
	for rows.Next() {
		var t TokenRow
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.ProjectName, &t.ProjectOpts,
			&t.Name, &t.Hash, &t.Prefix, &t.Scopes, &t.ExpiresAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Accounts -------------------------------------------------------------------

func (s *Store) CreateAccount(ctx context.Context, projectID, channel, label string, credsPlain []byte, limits json.RawMessage) (Account, error) {
	if limits == nil {
		limits = json.RawMessage(`{}`)
	}
	enc, err := crypto.EncryptSecretV3(s.MasterKey, credsPlain)
	if err != nil {
		return Account{}, err
	}
	var a Account
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO accounts(project_id, channel, label, creds_encrypted, limits)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, project_id, channel, label, creds_encrypted, limits`,
		projectID, channel, label, enc, limits,
	).Scan(&a.ID, &a.ProjectID, &a.Channel, &a.Label, &a.CredsEncrypted, &a.Limits)
	if err != nil {
		if isDuplicate(err) {
			return Account{}, fmt.Errorf("cuenta %q: %w", label, ErrConflict)
		}
		return Account{}, err
	}
	return a, nil
}

func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	var a Account
	err := s.Pool.QueryRow(ctx,
		`SELECT id, project_id, channel, label, creds_encrypted, limits FROM accounts WHERE id=$1`, id,
	).Scan(&a.ID, &a.ProjectID, &a.Channel, &a.Label, &a.CredsEncrypted, &a.Limits)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

func (s *Store) ListAccounts(ctx context.Context, projectID string) ([]Account, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT id, project_id, channel, label, creds_encrypted, limits FROM accounts WHERE project_id=$1 ORDER BY label`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Channel, &a.Label, &a.CredsEncrypted, &a.Limits); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAccount(ctx context.Context, id string) (projectID, label string, dests []string, err error) {
	// tx: leer los destinos que el CASCADE va a arrastrar y borrar deben ser
	// atómicos — el caller (admin API) audita esa pérdida y no puede leerla
	// a medias.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", "", nil, err
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx,
		`SELECT project_id::text, label FROM accounts WHERE id=$1`, id,
	).Scan(&projectID, &label); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", nil, ErrNotFound
		}
		return "", "", nil, err
	}
	rows, err := tx.Query(ctx, `SELECT name FROM destinations WHERE account_id=$1 ORDER BY name`, id)
	if err != nil {
		return "", "", nil, err
	}
	defer rows.Close() // CollectRows no cierra: un fallo a mitad de lectura no debe retener la conexión
	dests, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", "", nil, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM accounts WHERE id=$1`, id)
	if err != nil {
		return "", "", nil, err
	}
	if tag.RowsAffected() == 0 {
		return "", "", nil, ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", nil, err
	}
	return projectID, label, dests, nil
}

// openCreds prueba la cadena de claves/layouts sobre un blob: v3 (byte de
// versión), layout antiguo con cada clave en orden. La autenticación GCM
// desambigua (un fallo de clave o de layout es indistinguible de corrupción,
// así que se prueban todos los candidatos antes de rendirse).
func openCreds(sealed []byte, keys [][]byte) ([]byte, error) {
	for _, k := range keys {
		if len(k) == 0 {
			continue
		}
		if len(sealed) > 0 && sealed[0] == crypto.BlobVersionV3 {
			if pt, err := crypto.DecryptSecretV3(k, sealed); err == nil {
				return pt, nil
			}
		}
		if pt, err := crypto.DecryptSecret(k, sealed); err == nil {
			return pt, nil
		}
	}
	return nil, errors.New("creds no descifrables (¿master key correcta?)")
}

func (s *Store) DecryptCreds(a Account) ([]byte, error) {
	// cadena: v2 (actual) → v1 (PBKDF2 210k) → legacy (SHA256)
	return openCreds(a.CredsEncrypted, [][]byte{s.MasterKey, s.MidMasterKey, s.LegacyMasterKey})
}

// ReEncryptAccounts re-cifra TODAS las creds de cuentas con la clave actual en
// el formato v3. Con oldKeys no vacía descifra usando ESA cadena (rotación de
// passphrase: configúrala en RAVENHUB_MASTER_KEY_OLD, deja la nueva en
// RAVENHUB_MASTER_KEY y ejecuta `ravenhub re-encrypt`); con oldKeys vacía
// normaliza el formato (upgrade de filas pre-v3 sin rotar clave). Devuelve
// (re-cifradas, fallidas): las fallidas quedan intactas y el caller decide.
func (s *Store) ReEncryptAccounts(ctx context.Context, oldKeys [][]byte) (int, int, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT id, creds_encrypted FROM accounts WHERE creds_encrypted IS NOT NULL`)
	if err != nil {
		return 0, 0, err
	}
	type row struct {
		id   string
		blob []byte
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.blob); err != nil {
			rows.Close()
			return 0, 0, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	keys := oldKeys
	if len(keys) == 0 {
		keys = [][]byte{s.MasterKey, s.MidMasterKey, s.LegacyMasterKey}
	}
	ok, failed := 0, 0
	for _, r := range all {
		plain, err := openCreds(r.blob, keys)
		if err != nil {
			failed++
			continue
		}
		enc, err := crypto.EncryptSecretV3(s.MasterKey, plain)
		if err != nil {
			return ok, failed, err
		}
		if _, err := s.Pool.Exec(ctx,
			`UPDATE accounts SET creds_encrypted=$2 WHERE id=$1`, r.id, enc); err != nil {
			return ok, failed, err
		}
		ok++
	}
	return ok, failed, nil
}

// Destinations ---------------------------------------------------------------

func (s *Store) CreateDestination(ctx context.Context, projectID, name, accountID string, target, quietHours json.RawMessage) (Destination, error) {
	// el account debe pertenecer al proyecto (aislamiento por construcción)
	var acctProject string
	err := s.Pool.QueryRow(ctx, `SELECT project_id FROM accounts WHERE id=$1`, accountID).Scan(&acctProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return Destination{}, fmt.Errorf("cuenta %s: %w", accountID, ErrNotFound)
	}
	if err != nil {
		return Destination{}, err
	}
	if acctProject != projectID {
		return Destination{}, fmt.Errorf("la cuenta no pertenece al proyecto: %w", ErrConflict)
	}
	var d Destination
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO destinations(project_id, name, account_id, target, quiet_hours)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, project_id, name, account_id, target, quiet_hours`,
		projectID, name, accountID, target, quietHours,
	).Scan(&d.ID, &d.ProjectID, &d.Name, &d.AccountID, &d.Target, &d.QuietHours)
	if err != nil {
		if isDuplicate(err) {
			return Destination{}, fmt.Errorf("destino %q: %w", name, ErrConflict)
		}
		return Destination{}, err
	}
	return d, nil
}

func (s *Store) GetDestination(ctx context.Context, projectID, id string) (Destination, error) {
	var d Destination
	err := s.Pool.QueryRow(ctx, `
		SELECT id, project_id, name, account_id, target, quiet_hours
		FROM destinations WHERE id=$1 AND project_id=$2`, id, projectID,
	).Scan(&d.ID, &d.ProjectID, &d.Name, &d.AccountID, &d.Target, &d.QuietHours)
	if errors.Is(err, pgx.ErrNoRows) {
		return Destination{}, ErrNotFound
	}
	return d, err
}

func (s *Store) ListDestinations(ctx context.Context, projectID string) ([]Destination, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, project_id, name, account_id, target, quiet_hours
		FROM destinations WHERE project_id=$1 ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		var d Destination
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Name, &d.AccountID, &d.Target, &d.QuietHours); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetDestinationByID(ctx context.Context, id string) (Destination, error) {
	var d Destination
	err := s.Pool.QueryRow(ctx, `
		SELECT id, project_id, name, account_id, target, quiet_hours
		FROM destinations WHERE id=$1`, id,
	).Scan(&d.ID, &d.ProjectID, &d.Name, &d.AccountID, &d.Target, &d.QuietHours)
	if errors.Is(err, pgx.ErrNoRows) {
		return Destination{}, ErrNotFound
	}
	return d, err
}

func (s *Store) DeleteDestinationByID(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM destinations WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteDestination(ctx context.Context, projectID, id string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM destinations WHERE id=$1 AND project_id=$2`, id, projectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ResolveDestinations expands named destinations and groups within the
// project namespace, reporting names that could not be resolved.
func (s *Store) ResolveDestinations(ctx context.Context, projectID string, names []string) ([]ResolvedDestination, []string, error) {
	seen := map[string]bool{}
	var resolved []ResolvedDestination
	var missing []string

	expand := func(destIDs []string) error {
		for _, id := range destIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			var r ResolvedDestination
			err := s.Pool.QueryRow(ctx, `
				SELECT d.id, d.name, a.channel, d.target, COALESCE(d.quiet_hours,'null'::jsonb), a.id
				FROM destinations d JOIN accounts a ON a.id = d.account_id
				WHERE d.id=$1 AND d.project_id=$2`, id, projectID,
			).Scan(&r.DestID, &r.DestName, &r.Channel, &r.Target, &r.QuietHours, &r.AccountID)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			resolved = append(resolved, r)
		}
		return nil
	}

	for _, name := range names {
		var destID, groupID string
		destErr := s.Pool.QueryRow(ctx,
			`SELECT id FROM destinations WHERE project_id=$1 AND name=$2`, projectID, name,
		).Scan(&destID)
		groupErr := s.Pool.QueryRow(ctx,
			`SELECT id FROM destination_groups WHERE project_id=$1 AND name=$2`, projectID, name,
		).Scan(&groupID)
		if destErr != nil && !errors.Is(destErr, pgx.ErrNoRows) {
			return nil, nil, destErr
		}
		if groupErr != nil && !errors.Is(groupErr, pgx.ErrNoRows) {
			return nil, nil, groupErr
		}
		// Un mismo nombre como destino y grupo es ambiguo: un token scopeado
		// al grupo podría acabar enviando al destino homónimo (I-05).
		if destErr == nil && groupErr == nil {
			return nil, nil, fmt.Errorf("nombre ambiguo %q: existe como destino y como grupo", name)
		}
		if destErr == nil {
			if err := expand([]string{destID}); err != nil {
				return nil, nil, err
			}
			continue
		}
		if groupErr == nil {
			rows, err := s.Pool.Query(ctx, `
				SELECT destination_id FROM destination_group_members WHERE group_id=$1 ORDER BY destination_id`, groupID)
			if err != nil {
				return nil, nil, err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
			rows.Close()
			if err != nil {
				return nil, nil, err
			}
			if err := expand(ids); err != nil {
				return nil, nil, err
			}
			continue
		}
		missing = append(missing, name)
	}
	return resolved, missing, nil
}

// Groups ---------------------------------------------------------------------

func (s *Store) CreateGroup(ctx context.Context, projectID, name string, destNames []string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var groupID string
	err = tx.QueryRow(ctx, `
		INSERT INTO destination_groups(project_id, name) VALUES ($1,$2)
		RETURNING id`, projectID, name).Scan(&groupID)
	if err != nil {
		if isDuplicate(err) {
			return fmt.Errorf("grupo %q: %w", name, ErrConflict)
		}
		return err
	}
	for _, dn := range destNames {
		var destID string
		err := tx.QueryRow(ctx,
			`SELECT id FROM destinations WHERE project_id=$1 AND name=$2`, projectID, dn).Scan(&destID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("destino %q del grupo %q: %w", dn, name, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO destination_group_members(group_id, destination_id) VALUES ($1,$2)`, groupID, destID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) ListGroups(ctx context.Context, projectID string) ([]map[string]any, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT g.name,
		       COALESCE(json_agg(d.name ORDER BY d.name) FILTER (WHERE d.name IS NOT NULL), '[]'::jsonb)
		FROM destination_groups g
		LEFT JOIN destination_group_members m ON m.group_id = g.id
		LEFT JOIN destinations d ON d.id = m.destination_id AND d.project_id = g.project_id
		WHERE g.project_id=$1 GROUP BY g.name ORDER BY g.name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var name string
		var members json.RawMessage
		if err := rows.Scan(&name, &members); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"name": name, "destinations": members})
	}
	return out, rows.Err()
}

// DeleteGroup removes a named group (paridad con create/list).
func (s *Store) DeleteGroup(ctx context.Context, projectID, name string) error {
	tag, err := s.Pool.Exec(ctx,
		`DELETE FROM destination_groups WHERE project_id=$1 AND name=$2`, projectID, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Routing rules --------------------------------------------------------------

// nonNilSlice maps a nil slice to an empty one: las columnas TEXT[] de
// routing_rules son NOT NULL, y un match_tags ausente llegaba como NULL
// (violación 23502) — una regla sin tags es legítima (matchea por prioridad).
func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// UpsertRoutingRule inserts or updates a rule by (project, name). Declarativo:
// el seed puede declarar reglas.
func (s *Store) UpsertRoutingRule(ctx context.Context, projectID string, r model.RoutingRule) (model.RoutingRule, error) {
	payload, err := json.Marshal(r.MatchPayload)
	if err != nil {
		return model.RoutingRule{}, err
	}
	if payload == nil {
		payload = []byte(`{}`)
	}
	var id string
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO routing_rules(project_id, name, match_tags, match_priority,
		                          match_payload, destinations, stop_processing,
		                          enabled, priority)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (project_id, name) DO UPDATE
		SET match_tags=$3, match_priority=$4, match_payload=$5, destinations=$6,
		    stop_processing=$7, enabled=$8, priority=$9
		RETURNING id`,
		projectID, r.Name, nonNilSlice(r.MatchTags), nullIf(r.MatchPriority), payload,
		nonNilSlice(r.Destinations), r.StopProcessing, r.Enabled, r.Priority).Scan(&id)
	if err != nil {
		return model.RoutingRule{}, err
	}
	r.ID = id
	return r, nil
}

// CreateRoutingRule inserts a routing rule scoped to a project.
func (s *Store) CreateRoutingRule(ctx context.Context, projectID string, r model.RoutingRule) (model.RoutingRule, error) {
	payload, err := json.Marshal(r.MatchPayload)
	if err != nil {
		return model.RoutingRule{}, err
	}
	if payload == nil {
		payload = []byte(`{}`)
	}
	var id string
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO routing_rules(project_id, name, match_tags, match_priority,
		                          match_payload, destinations, stop_processing,
		                          enabled, priority)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id`,
		projectID, r.Name, nonNilSlice(r.MatchTags), nullIf(r.MatchPriority), payload,
		nonNilSlice(r.Destinations), r.StopProcessing, r.Enabled, r.Priority).Scan(&id)
	if err != nil {
		if isDuplicate(err) {
			return model.RoutingRule{}, fmt.Errorf("regla %q: %w", r.Name, ErrConflict)
		}
		return model.RoutingRule{}, err
	}
	r.ID = id
	return r, nil
}

// ListRoutingRules returns the project's rules in evaluation order
// (priority ASC, then creation), only the enabled-usable shape.
func (s *Store) ListRoutingRules(ctx context.Context, projectID string) ([]model.RoutingRule, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, name, match_tags, COALESCE(match_priority,''), match_payload,
		       destinations, stop_processing, enabled, priority
		FROM routing_rules WHERE project_id=$1
		ORDER BY priority, created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.RoutingRule
	for rows.Next() {
		var r model.RoutingRule
		var payload json.RawMessage
		if err := rows.Scan(&r.ID, &r.Name, &r.MatchTags, &r.MatchPriority, &payload,
			&r.Destinations, &r.StopProcessing, &r.Enabled, &r.Priority); err != nil {
			return nil, err
		}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &r.MatchPayload)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRoutingRule removes a rule by id.
func (s *Store) DeleteRoutingRule(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM routing_rules WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Templates ------------------------------------------------------------------

func (s *Store) UpsertTemplate(ctx context.Context, projectID, name, body string, overrides json.RawMessage) error {
	if overrides == nil {
		overrides = json.RawMessage(`{}`)
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO templates(project_id, name, body, overrides) VALUES ($1,$2,$3,$4)
		ON CONFLICT (project_id, name) DO UPDATE SET body=$3, overrides=$4`,
		projectID, name, body, overrides)
	return err
}

func (s *Store) GetTemplate(ctx context.Context, projectID, name string) (string, error) {
	var body string
	err := s.Pool.QueryRow(ctx,
		`SELECT body FROM templates WHERE project_id=$1 AND name=$2`, projectID, name).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return body, err
}

// Messages / cola ------------------------------------------------------------

// InsertOpts carries optional scheduling/aggregation behavior for EnqueueMessage.
type InsertOpts struct {
	// NextAttemptAt es el primer intento (cero → now()). send_at programado
	// y ventana de digest se traducen aquí por el API.
	NextAttemptAt time.Time
	// SendAt es el valor cronológico de referencia (columna send_at, audit).
	SendAt *time.Time
	// AggregationKey activa el anti-spam: si hay un digest abierto y bajo el
	// techo se une; si no, este mensaje abre el digest (is_digest=true).
	AggregationKey *string
	// Window es la ventana de agregación; el digest no se entrega hasta que
	// se cierra (next_attempt_at = ahora + window).
	Window time.Duration
}

// EnqueueResult describes what happened to one notify call.
type EnqueueResult struct {
	ID         string
	Created    bool // se creó un mensaje (o abrió un digest nuevo)
	Duplicate  bool // Idempotency-Key repetida → mismo id, sin reenviar
	Aggregated bool // se plegó en un digest abierto existente
	Count      int  // aggregated_count del digest tras el fold (0 = no aplica)
}

// EnqueueMessage es la puerta única de la cola: idempotencia + agregación +
// insert en una transacción con advisory lock por (proyecto, clave) para que
// dos primeros mensajes idénticos en paralelo no abran dos digests.
func (s *Store) EnqueueMessage(ctx context.Context, projectID, tokenID string, payload json.RawMessage, priority model.Priority, dedupKey *string, ttl time.Duration, opts InsertOpts) (EnqueueResult, error) {
	next := opts.NextAttemptAt
	if next.IsZero() {
		next = time.Now()
	}
	var sendAt any // nullable
	if opts.SendAt != nil {
		sendAt = *opts.SendAt
	}
	isDigest := opts.AggregationKey != nil

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return EnqueueResult{}, err
	}
	defer tx.Rollback(ctx)

	// Idempotencia SIEMPRE, también para mensajes que no agregan: una clave
	// plegada en un digest (folded_keys) debe seguir siendo duplicate aunque
	// el reintento llegue con prioridad high/urgent.
	if dedupKey != nil {
		var existing string
		err := tx.QueryRow(ctx, `
			SELECT id FROM messages
			WHERE project_id=$1 AND (
				dedup_key=$2
				OR (is_digest AND $2 = ANY(folded_keys))
			) LIMIT 1`, projectID, *dedupKey).Scan(&existing)
		if err == nil {
			return EnqueueResult{ID: existing, Duplicate: true}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return EnqueueResult{}, err
		}
	}

	if opts.AggregationKey != nil {
		// serializa los mensajes de la misma clave: un solo digest abierto
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtext($1 || ':' || $2))`,
			projectID, *opts.AggregationKey); err != nil {
			return EnqueueResult{}, err
		}
		// idempotencia dentro de la transacción (el índice único también
		// protege; esto además evita re-join del mismo key)
		if dedupKey != nil {
			var existing string
			err := tx.QueryRow(ctx, `
				SELECT id FROM messages
				WHERE project_id=$1 AND (
					dedup_key=$2
					OR (is_digest AND $2 = ANY(folded_keys))
				) LIMIT 1`,
				projectID, *dedupKey).Scan(&existing)
			if err == nil {
				return EnqueueResult{ID: existing, Duplicate: true}, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return EnqueueResult{}, err
			}
			// Sin filas: la clave no existe como dedup_key de ningún mensaje NI
			// como miembro plegado de ningún digest (abierto O cerrado — la
			// vida de fila de folded_keys es la misma que la de dedup_key).
			// Antes, un reintento con la clave de un miembro plegado a un
			// digest YA ENTREGADO creaba un mensaje nuevo → doble notificación.
		}
		// ¿digest abierto? (en cola, ventana abierta, bajo el techo)
		var digestID string
		err := tx.QueryRow(ctx, `
			SELECT id FROM messages
			WHERE project_id=$1 AND aggregation_key=$2
			  AND is_digest AND status='queued'
			  AND next_attempt_at > now()
			  AND aggregated_count < $3
			ORDER BY next_attempt_at LIMIT 1`,
			projectID, *opts.AggregationKey, model.DigestMaxCount).Scan(&digestID)
		switch {
		case err == nil:
			// idempotencia de lo ya plegado: la clave del mensaje que se fold-eó
			// vive en folded_keys del digest; un reintento con esa clave es
			// duplicate.
			if dedupKey != nil {
				var seen bool
				if err := tx.QueryRow(ctx,
					`SELECT $2::text = ANY(folded_keys) FROM messages WHERE id=$1`,
					digestID, *dedupKey).Scan(&seen); err != nil {
					return EnqueueResult{}, err
				}
				if seen {
					if err := tx.Commit(ctx); err != nil {
						return EnqueueResult{}, err
					}
					return EnqueueResult{ID: digestID, Duplicate: true}, nil
				}
			}
			var count int
			err = tx.QueryRow(ctx, `
				UPDATE messages SET aggregated_count=aggregated_count+1,
				                    payload=$2, updated_at=now(),
				                    folded_keys = CASE WHEN $3::text IS NULL THEN folded_keys
				                                       ELSE array_append(folded_keys, $3::text) END
				WHERE id=$1 AND is_digest AND status='queued'
				  AND next_attempt_at > now() AND aggregated_count < $4
				RETURNING aggregated_count`,
				digestID, payload, nullIf(deref(dedupKey)), model.DigestMaxCount).Scan(&count)
			switch {
			case err == nil:
				if err := tx.Commit(ctx); err != nil {
					return EnqueueResult{}, err
				}
				return EnqueueResult{ID: digestID, Aggregated: true, Count: count}, nil
			case errors.Is(err, pgx.ErrNoRows):
				// El digest se reclamó (o cerró su ventana) entre el SELECT y este
				// UPDATE: el UPDATE sin guardia plegaba sobre uno EN ENTREGA y la
				// copia se perdía en silencio (su clave quedaba en folded_keys de
				// un digest que ya llevaba el texto viejo). Cae al INSERT, que abre
				// un digest nuevo con este mensaje.
			default:
				return EnqueueResult{}, err
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return EnqueueResult{}, err
		}
	}

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO messages(project_id, token_id, payload, priority, dedup_key,
		                     expires_at, next_attempt_at, send_at,
		                     aggregation_key, aggregated_count, is_digest)
		VALUES ($1, NULLIF($2,'')::uuid, $3, $4, $5, now() + make_interval(secs => $6),
		        $7, $8, $9, 1, $10)
		ON CONFLICT (project_id, dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING
		RETURNING id`,
		projectID, tokenID, payload, string(priority), dedupKey, ttl.Seconds(),
		next, sendAt, opts.AggregationKey, isDigest,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// carrera de idempotencia: recuperar el id original
		err2 := tx.QueryRow(ctx,
			`SELECT id FROM messages WHERE project_id=$1 AND dedup_key=$2`,
			projectID, *dedupKey).Scan(&id)
		if err2 != nil {
			return EnqueueResult{}, err2
		}
		if err := tx.Commit(ctx); err != nil {
			return EnqueueResult{}, err
		}
		return EnqueueResult{ID: id, Duplicate: true}, nil
	}
	if err != nil {
		return EnqueueResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EnqueueResult{}, err
	}
	return EnqueueResult{ID: id, Created: true, Count: 1}, nil
}

// ClaimMessages atomically takes up to `batch` queued messages (SKIP LOCKED),
// moving them to 'delivering' and bumping attempts. Urgent/high van primero
// pero SIN hambruna: si quedan huecos se rellenan con el resto.
func (s *Store) ClaimMessages(ctx context.Context, batch int) ([]MessageRow, error) {
	claim := func(priorityFilter string, limit int) ([]MessageRow, error) {
		if limit <= 0 {
			return nil, nil
		}
		rows, err := s.Pool.Query(ctx, `
			UPDATE messages SET status='delivering', attempts=attempts+1, updated_at=now()
			WHERE id IN (
				SELECT id FROM messages
				WHERE status='queued' AND next_attempt_at <= now()
				  AND expires_at > now()`+priorityFilter+`
				ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED
			)
			RETURNING id, project_id, payload, priority, status, dedup_key, expires_at, attempts, next_attempt_at, created_at, updated_at,
			          send_at, aggregation_key, aggregated_count, is_digest`,
			limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return pgx.CollectRows(rows, pgx.RowToStructByPos[MessageRow])
	}
	msgs, err := claim(` AND priority IN ('urgent','high')`, batch)
	if err != nil {
		return nil, err
	}
	if len(msgs) >= batch {
		return msgs, nil
	}
	rest, err := claim("", batch-len(msgs))
	if err != nil {
		return msgs, err
	}
	return append(msgs, rest...), nil
}

// RequeueStale returns 'delivering' messages whose lease expired to the queue
// (crash recovery: matar el proceso no pierde mensajes).
func (s *Store) RequeueStale(ctx context.Context, lease time.Duration) (int, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE messages SET status='queued', next_attempt_at=now(), updated_at=now()
		WHERE status='delivering' AND updated_at < now() - make_interval(secs => $1)`,
		lease.Seconds())
	return int(tag.RowsAffected()), err
}

// ExpiredMessage identifies one message retired by TTL.
type ExpiredMessage struct {
	ID        string
	ProjectID string
	Priority  model.Priority
}

// ExpireMessages retires messages whose TTL is gone (lo viejo expira).
// Devuelve los mensajes expirados (id/proyecto/prioridad) para que el worker
// deje rastro (métrica + auditoría): una pérdida por TTL no puede ser muda.
// Los 'delivering' con latido fresco (updated_at dentro del lease) NO se
// tocan: el TTL acota la espera en cola, no corta una entrega viva. Sin esta
// guardia el expirer podía marcar 'expired' un mensaje a mitad de entrega y
// perder la copia diferida de quiet hours al reencolarla.
func (s *Store) ExpireMessages(ctx context.Context, lease time.Duration) ([]ExpiredMessage, error) {
	rows, err := s.Pool.Query(ctx, `
		UPDATE messages SET status='expired', updated_at=now()
		WHERE status IN ('queued','delivering') AND expires_at < now()
		  AND (status='queued' OR updated_at < now() - make_interval(secs => $1))
		RETURNING id, project_id, priority`, lease.Seconds())
	if err != nil {
		return nil, err
	}
	var out []ExpiredMessage
	for rows.Next() {
		var e ExpiredMessage
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.Priority); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(out))
	for _, e := range out {
		ids = append(ids, e.ID)
	}
	_, err = s.Pool.Exec(ctx, `
		UPDATE deliveries SET status='failed', error='expirado (TTL)', updated_at=now()
		WHERE message_id = ANY($1::uuid[]) AND status='pending'`, ids)
	return out, err
}

func (s *Store) SetMessageRetry(ctx context.Context, id string, next time.Time) error {
	// Guardia: solo un mensaje EN ENTREGA (reclamado por este worker) vuelve a
	// la cola. Sin ella, un reschedule tras una expiración concurrente
	// resucitaba un mensaje ya terminal.
	_, err := s.Pool.Exec(ctx, `
		UPDATE messages SET status='queued', next_attempt_at=$2, updated_at=now() WHERE id=$1 AND status='delivering'`,
		id, next)
	return err
}

// QuietRequeue reschedules a message deferred by quiet hours WITHOUT
// spending a retry attempt (the claim already incremented it). El aplazamiento
// tampoco consume TTL: la ventana de vida se desplaza con el diferido para
// que una notificación normal (TTL 1h) no expire antes de salir.
func (s *Store) QuietRequeue(ctx context.Context, id string, next time.Time) error {
	// Guardia: solo un mensaje EN ENTREGA se reprograma por quiet hours/pacing.
	// Sin ella, QuietRequeue podía resucitar un mensaje que el expirer ya marcó
	// 'expired' a mitad de entrega. Con 0 filas afectadas el worker simplemente
	// no reprograma.
	_, err := s.Pool.Exec(ctx, `
		UPDATE messages SET status='queued', next_attempt_at=$2, updated_at=now(),
		                    attempts = GREATEST(attempts-1, 0),
		                    expires_at = LEAST(GREATEST(expires_at, now()) + GREATEST($2 - now(), interval '0'),
		                                       created_at + interval '7 days')
		WHERE id=$1 AND status='delivering'`, id, next)
	return err
}

// FinalizeMessage sets the terminal status of a message whose deliveries are
// all done: 'sent' if none failed, 'failed' otherwise. Returns the new status,
// or "" if pending deliveries remain.
func (s *Store) FinalizeMessage(ctx context.Context, id string) (string, error) {
	var status string
	err := s.Pool.QueryRow(ctx, `
		UPDATE messages SET status = (
			SELECT CASE WHEN count(*) FILTER (WHERE status='failed') > 0 THEN 'failed' ELSE 'sent' END
			FROM deliveries WHERE message_id=$1
		), updated_at=now()
		WHERE id=$1 AND status IN ('queued','delivering')
		  AND NOT EXISTS (SELECT 1 FROM deliveries WHERE message_id=$1 AND status='pending')
		RETURNING status`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // aún hay pendientes (otro worker avanzó)
	}
	return status, err
}

// SetMessageStatus fuerza un estado (solo lo usa el worker para mandar a DLQ).
// Guardia: solo transiciones desde estados vivos. Un mensaje que el expirer
// ya marcó terminal no puede ser sobreescrito a 'dead' por un worker que
// todavía no se enteró.
func (s *Store) SetMessageStatus(ctx context.Context, id, status string) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE messages SET status=$2, updated_at=now() WHERE id=$1 AND status IN ('queued','delivering')`, id, status)
	return err
}

// ReplayMessage revives a dead/failed/expired message for another round.
// El TTL del replay respeta los overrides por proyecto {"ttl": {...}} igual
// que el intake.
func (s *Store) ReplayMessage(ctx context.Context, id string) (MessageRow, error) {
	var priority model.Priority
	var opts json.RawMessage
	if err := s.Pool.QueryRow(ctx, `
		SELECT m.priority, COALESCE(p.options,'{}'::jsonb)
		FROM messages m LEFT JOIN projects p ON p.id = m.project_id
		WHERE m.id=$1`, id).Scan(&priority, &opts); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MessageRow{}, ErrNotFound
		}
		return MessageRow{}, err
	}
	ttl := model.TTLFor(priority, opts)
	var m MessageRow
	err := s.Pool.QueryRow(ctx, `
		UPDATE messages
		SET status='queued', attempts=0, next_attempt_at=now(),
		    expires_at=now() + make_interval(secs => $2),
		    updated_at=now()
		WHERE id=$1 AND status IN ('dead','failed','expired')
		RETURNING id, project_id, payload, priority, status, dedup_key, expires_at, attempts, next_attempt_at, created_at, updated_at,
		          send_at, aggregation_key, aggregated_count, is_digest`, id, ttl.Seconds(),
	).Scan(&m.ID, &m.ProjectID, &m.Payload, &m.Priority, &m.Status, &m.DedupKey,
		&m.ExpiresAt, &m.Attempts, &m.NextAttemptAt, &m.CreatedAt, &m.UpdatedAt,
		&m.SendAt, &m.AggregationKey, &m.AggregatedCount, &m.IsDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageRow{}, ErrNotFound
	}
	if err != nil {
		return MessageRow{}, err
	}
	_, err = s.Pool.Exec(ctx, `
		UPDATE deliveries SET status='pending', error=NULL, updated_at=now()
		WHERE message_id=$1 AND status IN ('failed')`, id)
	return m, err
}

func (s *Store) GetMessage(ctx context.Context, projectID *string, id string) (MessageRow, error) {
	q := `SELECT id, project_id, payload, priority, status, dedup_key, expires_at, attempts, next_attempt_at, created_at, updated_at,
	             send_at, aggregation_key, aggregated_count, is_digest
	      FROM messages WHERE id=$1`
	args := []any{id}
	if projectID != nil {
		q += ` AND project_id=$2`
		args = append(args, *projectID)
	}
	var m MessageRow
	err := s.Pool.QueryRow(ctx, q, args...).
		Scan(&m.ID, &m.ProjectID, &m.Payload, &m.Priority, &m.Status, &m.DedupKey,
			&m.ExpiresAt, &m.Attempts, &m.NextAttemptAt, &m.CreatedAt, &m.UpdatedAt,
			&m.SendAt, &m.AggregationKey, &m.AggregatedCount, &m.IsDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return MessageRow{}, ErrNotFound
	}
	return m, err
}

// ListMessages lista mensajes (orden created_at DESC). scopeDestinations
// filtra EN SQL por intersección del "to" del payload con los scopes del
// token: filtrar tras el LIMIT (en Go) ocultaba mensajes visibles más allá
// de la página a los tokens acotados. nil/empty = todo el proyecto.
func (s *Store) ListMessages(ctx context.Context, projectID *string, status string, limit int, scopeDestinations []string) ([]MessageRow, error) {
	q := `SELECT id, project_id, payload, priority, status, dedup_key, expires_at, attempts, next_attempt_at, created_at, updated_at,
	             send_at, aggregation_key, aggregated_count, is_digest
	      FROM messages WHERE true`
	args := []any{}
	if projectID != nil {
		args = append(args, *projectID)
		q += fmt.Sprintf(` AND project_id=$%d`, len(args))
	}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(` AND status=$%d`, len(args))
	}
	if len(scopeDestinations) > 0 {
		args = append(args, scopeDestinations)
		q += fmt.Sprintf(` AND payload->'to' ?| $%d::text[]`, len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT $%d`, len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		var m MessageRow
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Payload, &m.Priority, &m.Status, &m.DedupKey,
			&m.ExpiresAt, &m.Attempts, &m.NextAttemptAt, &m.CreatedAt, &m.UpdatedAt,
			&m.SendAt, &m.AggregationKey, &m.AggregatedCount, &m.IsDigest); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Deliveries -----------------------------------------------------------------

// EnsureDeliveries creates pending delivery rows for the resolved set.
func (s *Store) EnsureDeliveries(ctx context.Context, messageID string, dests []ResolvedDestination) error {
	for _, d := range dests {
		if _, err := s.Pool.Exec(ctx, `
			INSERT INTO deliveries(message_id, destination_id, dest_name, channel)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (message_id, dest_name) DO UPDATE
			   SET destination_id=EXCLUDED.destination_id
			 WHERE deliveries.status='pending' AND deliveries.destination_id IS NULL`,
			messageID, d.DestID, d.DestName, d.Channel); err != nil {
			return err
		}
	}
	return nil
}

// PendingDeliveries returns the unsent deliveries of a message, joined with
// the current destination/account state (nil join fields = destino borrado).
func (s *Store) PendingDeliveries(ctx context.Context, messageID string) ([]DeliveryRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT dv.id, dv.dest_name, dv.channel, dv.status, dv.error, dv.provider_msg_id, dv.latency_ms, dv.truncated,
		       d.id, d.target, COALESCE(d.quiet_hours,'null'::jsonb), a.id, a.creds_encrypted, a.channel
		FROM deliveries dv
		LEFT JOIN destinations d ON d.id = dv.destination_id
		LEFT JOIN accounts a ON a.id = d.account_id
		WHERE dv.message_id=$1 AND dv.status='pending'`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeliveryRow
	for rows.Next() {
		var v DeliveryRow
		if err := rows.Scan(&v.ID, &v.DestName, &v.Channel, &v.Status, &v.Error, &v.ProviderMsgID, &v.LatencyMs, &v.Truncated,
			&v.DestID, &v.Target, &v.QuietHours, &v.AccountID, &v.CredsEnc, &v.AccountCh); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) SetDeliverySent(ctx context.Context, id, providerMsgID string, latencyMs int, truncated bool) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE deliveries SET status='sent', provider_msg_id=$2, latency_ms=$3, truncated=$4, error=NULL, updated_at=now()
		WHERE id=$1`, id, providerMsgID, latencyMs, truncated)
	return err
}

// SetDeliveryFailed records a permanent failure. truncated refleja el render
// del canal y se persiste también en el fallo.
func (s *Store) SetDeliveryFailed(ctx context.Context, id, errMsg string, truncated bool) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE deliveries SET status='failed', error=$2, truncated=$3, updated_at=now() WHERE id=$1`, id, errMsg, truncated)
	return err
}

// NoteDeliveryRetryable records the last transient error, delivery stays pending.
func (s *Store) NoteDeliveryRetryable(ctx context.Context, id, errMsg string) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE deliveries SET error=$2, updated_at=now() WHERE id=$1`, id, errMsg)
	return err
}

// FailPendingDeliveries cierra las entregas aún pendientes de un mensaje que
// va directo a DLQ (p.ej. destinos eliminados): sin esto quedaban filas
// 'pending' huérfanas colgando de un mensaje 'dead' para siempre.
func (s *Store) FailPendingDeliveries(ctx context.Context, messageID, errMsg string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE deliveries SET status='failed', error=$2, updated_at=now()
		WHERE message_id=$1 AND status='pending'`, messageID, errMsg)
	return err
}

func (s *Store) ListDeliveries(ctx context.Context, messageID string) ([]DeliveryRow, error) {
	// La proyección debe casar columna a columna con el scan de abajo
	// (DestID, Target, QuietHours, AccountID, CredsEnc): con NULLs sin tipar
	// "funcionaba" solo porque pgx asigna nil sin mirar el tipo.
	rows, err := s.Pool.Query(ctx, `
		SELECT id, dest_name, channel, status, error, provider_msg_id, latency_ms, truncated,
		       NULL, NULL::jsonb, NULL::jsonb, NULL, NULL::bytea
		FROM deliveries WHERE message_id=$1 ORDER BY created_at`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeliveryRow
	for rows.Next() {
		var v DeliveryRow
		if err := rows.Scan(&v.ID, &v.DestName, &v.Channel, &v.Status, &v.Error, &v.ProviderMsgID, &v.LatencyMs, &v.Truncated,
			&v.DestID, &v.Target, &v.QuietHours, &v.AccountID, &v.CredsEnc); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Audit ----------------------------------------------------------------------

func (s *Store) InsertAudit(ctx context.Context, projectID *string, tokenPrefix, action, subject string, detail any) {
	var det json.RawMessage
	if detail != nil {
		det, _ = json.Marshal(detail)
	}
	// Best-effort, pero no rehén del cliente: el ctx de la petición se cancela
	// en cuanto el agente corta la conexión (fire-and-forget típico), y la
	// side effect ya está confirmada — la auditoría post-commit merece su
	// propio presupuesto, con la cancelación del caller desacoplada.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if _, err := s.Pool.Exec(actx, `
		INSERT INTO audit_log(project_id, token_prefix, action, subject, detail)
		VALUES (NULLIF($1,'')::uuid, $2, $3, $4, $5)`,
		deref(projectID), tokenPrefix, action, subject, det); err != nil {
		// un fallo de auditoría no tumba la operación, pero no puede ser mudo
		slog.Warn("auditoría no registrada", "action", action, "err", err)
	}
}

// AuditEntry is one row for InsertAuditBatch.
type AuditEntry struct {
	ProjectID   *string
	TokenPrefix string
	Action      string
	Subject     string
	Detail      any
}

// InsertAuditBatch inserts many audit rows in a single statement (el expirer
// puede retirar cientos de mensajes de golpe: una sentencia, no N), con el
// mismo presupuesto desacoplado del ctx del caller que InsertAudit.
func (s *Store) InsertAuditBatch(ctx context.Context, entries []AuditEntry) {
	if len(entries) == 0 {
		return
	}
	projects := make([]string, len(entries))
	prefixes := make([]string, len(entries))
	actions := make([]string, len(entries))
	subjects := make([]string, len(entries))
	details := make([]string, len(entries))
	for i, e := range entries {
		projects[i] = deref(e.ProjectID)
		prefixes[i] = e.TokenPrefix
		actions[i] = e.Action
		subjects[i] = e.Subject
		if e.Detail != nil {
			if b, err := json.Marshal(e.Detail); err == nil {
				details[i] = string(b)
				continue
			}
		}
		details[i] = "{}"
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.Pool.Exec(actx, `
		INSERT INTO audit_log(project_id, token_prefix, action, subject, detail)
		SELECT NULLIF(p,'')::uuid, pr, a, su, d::jsonb
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[]) AS t(p, pr, a, su, d)`,
		projects, prefixes, actions, subjects, details); err != nil {
		slog.Warn("auditoría batch no registrada", "n", len(entries), "err", err)
	}
}

func (s *Store) ListAudit(ctx context.Context, projectID *string, limit int) ([]map[string]any, error) {
	q := `SELECT id, COALESCE(project_id::text,''), COALESCE(token_prefix,''), action, COALESCE(subject,''),
	             COALESCE(detail::text,'{}'), created_at FROM audit_log WHERE true`
	args := []any{}
	if projectID != nil {
		args = append(args, *projectID)
		q += fmt.Sprintf(` AND project_id=$%d`, len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id int64
		var pid, prefix, action, subject, detail string
		var created time.Time
		if err := rows.Scan(&id, &pid, &prefix, &action, &subject, &detail, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "project_id": pid, "token_prefix": prefix,
			"action": action, "subject": subject, "detail": detail, "created_at": created,
		})
	}
	return out, rows.Err()
}

// Stats ----------------------------------------------------------------------

func (s *Store) CountByStatus(ctx context.Context, status string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE status=$1`, status).Scan(&n)
	return n, err
}

// CountPending cuenta el trabajo vivo del hub (queued + delivering) para la
// contrapresión global. Excluye los programados a más de 1h: no consumen
// workers ahora y un tenant no puede llenar el hard global con send_at lejanos
// El readyz sigue mostrando el trabajo realmente inminente.
func (s *Store) CountPending(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `
		SELECT count(*) FROM messages
		WHERE status IN ('queued','delivering')
		  AND (send_at IS NULL OR send_at <= now() + interval '1 hour')`).Scan(&n)
	return n, err
}

// CountByStatusForProject cuenta por estado dentro de UN proyecto: las stats
// globales suman el historial de todos los proyectos de la DB y falsean los
// umbrales por tenant (p. ej. el drenaje de una ráfaga propia).
func (s *Store) CountByStatusForProject(ctx context.Context, projectID, status string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE project_id=$1 AND status=$2`, projectID, status).Scan(&n)
	return n, err
}

// CountQueuedForProject cuenta queued+delivering de un proyecto para el
// shedding por tenant: un proyecto no puede consumir la cuota global y
// degradar a los demás. Excluye programados a más de 1h, igual que
// CountPending: sin ello, 2000 send_at lejanos bloqueaban al proyecto con la
// cola inminente vacía.
func (s *Store) CountQueuedForProject(ctx context.Context, projectID string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `
		SELECT count(*) FROM messages
		WHERE project_id=$1 AND status IN ('queued','delivering')
		  AND (send_at IS NULL OR send_at <= now() + interval '1 hour')`, projectID).Scan(&n)
	return n, err
}

// TouchMessages refreshes updated_at for still-delivering messages (latido
// de lease): evita que el reaper reclame el resto de un lote lento.
func (s *Store) TouchMessages(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.Pool.Exec(ctx,
		`UPDATE messages SET updated_at=now() WHERE id = ANY($1::uuid[]) AND status='delivering'`, ids)
	return err
}

// DeleteOldMessages removes TERMINAL messages older than cutoff (retención).
// Lo vivo (queued/delivering) jamás se borra. Por updated_at para no borrar
// replays recientes.
func (s *Store) DeleteOldMessages(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `
		DELETE FROM messages
		WHERE updated_at < $1 AND status IN ('sent','failed','dead','expired')`, cutoff)
	return tag.RowsAffected(), err
}

// DeleteOldAudit removes audit rows older than cutoff. audit_log es
// append-only por trigger: el borrado solo procede dentro de una transacción
// marcada como barrido de retención (ver migración 0002).
func (s *Store) DeleteOldAudit(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('ravenhub.retention_sweep', 'on', true)`); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM audit_log WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// isDuplicate detecta violación de unicidad por SQLSTATE 23505: por código,
// no por texto del error (que cambia con el locale del servidor).
const pgUniqueViolation = "23505"

func isDuplicate(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

// nullIf maps an empty string to NULL for nullable columns.
func nullIf(s string) any {
	if s == "" {
		return nil
	}
	return s
}
