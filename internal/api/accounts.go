package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"jellyscale/internal/quickconnect"
	"jellyscale/internal/signin"
)

// maxAccountBody bounds the body of a user management request.
const maxAccountBody = 1 << 16

// userName is what Jellyfin 12.4 takes as a name: letters, digits and a few marks, with no space at either end
// (checked apart).
var userName = regexp.MustCompile(`^[\p{L}\p{M}\p{Nd}\p{Pc} \-'._@+]+$`)

const invalidName = "a user name is letters, digits, spaces and - ' . _ @ +, with no space at either end"

const nameTaken = "a user with that name already exists"

func validUserName(name string) bool {
	return userName.MatchString(name) && strings.TrimSpace(name) == name
}

// accountRefusal is a change to an account that is refused, with what the caller is told.
type accountRefusal struct {
	status int
	reason string
}

func (refusal accountRefusal) Error() string { return refusal.reason }

// finishAccountChange answers a change to an account: 204, or why not.
func (s *Server) finishAccountChange(w http.ResponseWriter, r *http.Request, err error) {
	var refusal accountRefusal
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.As(err, &refusal):
		http.Error(w, refusal.reason, refusal.status)
	case errors.Is(err, pgx.ErrNoRows):
		notFound(w, r)
	default:
		s.fail(w, r, err)
	}
}

func readAccountBody(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAccountBody)).Decode(into); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return false
	}
	return true
}

// listUsers answers a viewer with their own account alone, and an administrator with every account.
func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	caller := sessionOf(r).User
	if !caller.IsAdmin {
		writeJSON(w, []any{s.userDTO(caller)})
		return
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT "+userColumns+" FROM users u ORDER BY lower(u.name)")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	users, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (User, error) {
		var user User
		return user, row.Scan(user.fields()...)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hidden, disabled := queryBool(r, "ishidden"), queryBool(r, "isdisabled")
	listed := []any{}
	for _, user := range users {
		// No account is hidden.
		if (hidden != nil && *hidden) || (disabled != nil && *disabled != user.IsDisabled) {
			continue
		}
		listed = append(listed, s.userDTO(user))
	}
	writeJSON(w, listed)
}

// newUser makes an enabled account that is no administrator. With no password, it has none (D-105).
func (s *Server) newUser(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name, Password string }
	if !readAccountBody(w, r, &body) {
		return
	}
	if !validUserName(body.Name) {
		http.Error(w, invalidName, http.StatusBadRequest)
		return
	}
	hash := ""
	if body.Password != "" {
		if reason := signin.Unacceptable(body.Password); reason != "" {
			http.Error(w, reason, http.StatusBadRequest)
			return
		}
		var err error
		if hash, err = hashPassword(body.Password); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	var user User
	err := s.Pool.QueryRow(r.Context(), `INSERT INTO users AS u (id, name, password_hash) VALUES ($1, $2, $3)
		ON CONFLICT (lower(name)) DO NOTHING RETURNING `+userColumns, uuid.NewString(), body.Name, hash).Scan(user.fields()...)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, nameTaken, http.StatusBadRequest)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Log.Info("user created", "user", user.Name, "by", sessionOf(r).User.Name)
	writeJSON(w, s.userDTO(user))
}

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

// isConfiguredAdmin says whether the configuration names the account.
func (s *Server) isConfiguredAdmin(name string) bool {
	return s.AdminUser != "" && strings.EqualFold(name, s.AdminUser)
}

// refuseConfigured is the refusal to delete or rename the configured administrator (D-095).
func (s *Server) refuseConfigured(name, change string) error {
	s.Log.Warn("refused to "+change+" the account the adminUser key names", "user", name)
	return accountRefusal{http.StatusForbidden, "the adminUser key of the configuration names this account; remove the key to " + change + " it"}
}

// updateUser saves the name and, when the body has one, the Configuration. It reads no Policy.
func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          string
		Configuration json.RawMessage
	}
	if !readAccountBody(w, r, &body) {
		return
	}
	var configuration *string
	if len(body.Configuration) > 0 && string(body.Configuration) != "null" {
		parsed, ok := parseConfiguration(body.Configuration)
		if !ok {
			http.Error(w, "invalid Configuration", http.StatusBadRequest)
			return
		}
		stored, _ := json.Marshal(parsed)
		text := string(stored)
		configuration = &text
	}
	s.finishAccountChange(w, r, s.saveUser(r.Context(), sessionOf(r).Target, body.Name, configuration))
}

func (s *Server) saveUser(ctx context.Context, target User, name string, configuration *string) error {
	// An existing name is kept as it is, whatever it holds.
	if name != target.Name {
		if s.isConfiguredAdmin(target.Name) {
			return s.refuseConfigured(target.Name, "rename")
		}
		if !validUserName(name) {
			return accountRefusal{http.StatusBadRequest, invalidName}
		}
	}
	tag, err := s.Pool.Exec(ctx, "UPDATE users SET name = $2, configuration = COALESCE($3::jsonb, configuration) WHERE id = $1::uuid",
		target.ID, name, configuration)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return accountRefusal{http.StatusBadRequest, nameTaken}
	}
	if err == nil && tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}

// lockAccount reads the account and holds its row, after the rows of the enabled administrators, which every change
// that could leave none holds in the same order: of two such changes at once, the second sees what the first left.
// lastAdmin says the account is the only enabled administrator. The lock leaves foreign key checks alone.
func lockAccount(ctx context.Context, tx pgx.Tx, id string) (account User, lastAdmin bool, err error) {
	rows, err := tx.Query(ctx, "SELECT id::text FROM users WHERE is_admin AND NOT is_disabled ORDER BY id FOR NO KEY UPDATE")
	if err != nil {
		return account, false, err
	}
	admins, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return account, false, err
	}
	err = tx.QueryRow(ctx, "SELECT "+userColumns+" FROM users u WHERE u.id = $1::uuid FOR NO KEY UPDATE", id).Scan(account.fields()...)
	return account, len(admins) == 1 && admins[0] == account.ID, err
}

const lastAdministrator = "there must be at least one enabled administrator"

// deleteUser removes the account and everything it holds, in one transaction.
func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id := sessionOf(r).Target.ID
	var name string
	err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		// The codes have no foreign key; every other row goes with the user's.
		if err := quickconnect.Forget(r.Context(), tx, id, ""); err != nil {
			return err
		}
		account, lastAdmin, err := lockAccount(r.Context(), tx, id)
		if err != nil {
			return err
		}
		if s.isConfiguredAdmin(account.Name) {
			return s.refuseConfigured(account.Name, "delete")
		}
		if lastAdmin {
			return accountRefusal{http.StatusForbidden, lastAdministrator}
		}
		name = account.Name
		_, err = tx.Exec(r.Context(), "DELETE FROM users WHERE id = $1::uuid", id)
		return err
	})
	if err == nil {
		s.Log.Info("user deleted", "user", name, "by", sessionOf(r).User.Name)
	}
	s.finishAccountChange(w, r, err)
}

// enforcedPolicy are the fields of a Policy that are stored and act.
var enforcedPolicy = []string{"IsAdministrator", "IsDisabled", "EnableContentDownloading"}

// channelPolicy are fields about channels, of which there are none, so any value of them says the same.
var channelPolicy = []string{"EnableAllChannels", "EnabledChannels", "BlockedChannels"}

// setPolicy changes the enforced fields the body names, and refuses a change to any other (D-093, D-094).
func (s *Server) setPolicy(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if !readAccountBody(w, r, &body) {
		return
	}
	for _, field := range enforcedPolicy {
		value, named := body[field]
		if _, isBool := value.(bool); named && !isBool {
			http.Error(w, field+" must be true or false", http.StatusBadRequest)
			return
		}
	}
	id := sessionOf(r).Target.ID
	var changed User
	err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) (err error) {
		changed, err = s.changePolicy(r.Context(), tx, id, body)
		return err
	})
	if err == nil {
		s.Log.Info("user policy saved", "user", changed.Name, "by", sessionOf(r).User.Name,
			"administrator", changed.IsAdmin, "disabled", changed.IsDisabled, "downloads", changed.AllowDownload)
	}
	s.finishAccountChange(w, r, err)
}

func (s *Server) changePolicy(ctx context.Context, tx pgx.Tx, id string, body map[string]any) (User, error) {
	disabling := body["IsDisabled"] == true
	if disabling {
		if err := quickconnect.Forget(ctx, tx, id, ""); err != nil {
			return User{}, err
		}
	}
	account, lastAdmin, err := lockAccount(ctx, tx, id)
	if err != nil {
		return account, err
	}
	unenforced, err := unenforcedChanges(policyOf(account), body)
	if err != nil {
		return account, err
	}
	if len(unenforced) > 0 {
		fields := strings.Join(unenforced, ", ")
		s.Log.Warn("a Policy change was refused: the fields are not enforced", "user", account.Name, "fields", fields)
		return account, accountRefusal{http.StatusBadRequest, "these Policy fields are not enforced, so they cannot be changed: " + fields}
	}
	for field, stored := range map[string]*bool{"IsAdministrator": &account.IsAdmin, "IsDisabled": &account.IsDisabled, "EnableContentDownloading": &account.AllowDownload} {
		if value, named := body[field].(bool); named {
			*stored = value
		}
	}
	if lastAdmin && (!account.IsAdmin || account.IsDisabled) {
		return account, accountRefusal{http.StatusForbidden, lastAdministrator}
	}
	if _, err := tx.Exec(ctx, "UPDATE users SET is_admin = $2, is_disabled = $3, allow_download = $4 WHERE id = $1::uuid",
		id, account.IsAdmin, account.IsDisabled, account.AllowDownload); err != nil {
		return account, err
	}
	if !disabling {
		return account, nil
	}
	return account, signin.EndTokens(ctx, tx, id, "")
}

// unenforcedChanges names, in order, the fields of body that are not enforced and differ from what the Policy answers.
// null says the same as an empty list, or as a field the Policy does not have.
func unenforcedChanges(policy, body map[string]any) ([]string, error) {
	// As a client reads it, so that values compare as JSON does.
	raw, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	var answered map[string]any
	if err := json.Unmarshal(raw, &answered); err != nil {
		return nil, err
	}
	var changes []string
	for field, value := range body {
		if slices.Contains(enforcedPolicy, field) || slices.Contains(channelPolicy, field) {
			continue
		}
		want, known := answered[field]
		list, isList := want.([]any)
		if value == nil && (!known || isList && len(list) == 0) {
			continue
		}
		if !known || !reflect.DeepEqual(value, want) {
			changes = append(changes, field)
		}
	}
	slices.Sort(changes)
	return changes, nil
}

const wrongCurrentPassword = "Invalid user or password entered."

// changePassword sets a password. A viewer, and an administrator changing their own, proves the current one; an
// administrator sets another's without it, or resets it to none (D-103). The account's other tokens end with it.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPw, NewPw string
		ResetPassword    bool
	}
	if !readAccountBody(w, r, &body) {
		return
	}
	session := sessionOf(r)
	own := sameUser(session.Target.ID, session.User.ID)
	if body.ResetPassword && own {
		http.Error(w, "your own password cannot be reset; change it, or ask another administrator", http.StatusForbidden)
		return
	}
	change := passwordChange{userID: session.Target.ID}
	if !body.ResetPassword {
		if reason := signin.Unacceptable(body.NewPw); reason != "" {
			http.Error(w, reason, http.StatusBadRequest)
			return
		}
		if own {
			checked, ok := s.checkCurrentPassword(w, r, body.CurrentPw)
			if !ok {
				return
			}
			change.checkedHash, change.keep = checked, session.Token
		}
		var err error
		if change.hash, err = hashPassword(body.NewPw); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	err := s.storePassword(r.Context(), change)
	if err == nil {
		s.Log.Info("password saved", "user", session.Target.Name, "by", session.User.Name, "reset", body.ResetPassword)
	}
	s.finishAccountChange(w, r, err)
}

// checkCurrentPassword compares the caller's current password within the sign-in bounds, as the device that asks. It
// returns the hash it matched, and has answered the request when it returns false.
func (s *Server) checkCurrentPassword(w http.ResponseWriter, r *http.Request, password string) (string, bool) {
	session := sessionOf(r)
	var stored, device string
	if err := s.Pool.QueryRow(r.Context(), `SELECT u.password_hash, d.device_id FROM devices d JOIN users u ON u.id = d.user_id
		WHERE d.token = $1`, session.Token).Scan(&stored, &device); err != nil {
		s.fail(w, r, err)
		return "", false
	}
	ticket, ok := s.reserveAttempt(w, r, signin.Attempt{Name: session.User.Name, UserID: session.User.ID, DeviceID: device}, session.User.Name)
	if !ok {
		return "", false
	}
	if !passwordMatches(stored, password) {
		signinFailures.Inc()
		http.Error(w, wrongCurrentPassword, http.StatusForbidden)
		return "", false
	}
	if err := ticket.Release(r.Context(), s.Pool); err != nil {
		s.fail(w, r, err)
		return "", false
	}
	return stored, true
}

// passwordChange is a password to store. An empty hash is no password. When the account changes its own, checkedHash is
// what its current password matched and keep is its token, which stays.
type passwordChange struct {
	userID, hash, checkedHash, keep string
}

func (s *Server) storePassword(ctx context.Context, change passwordChange) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := quickconnect.Forget(ctx, tx, change.userID, change.keep); err != nil {
			return err
		}
		// An account disabled, or given another password, since the caller proved theirs stores nothing.
		tag, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2
			WHERE id = $1::uuid AND ($3 = '' OR (password_hash = $3 AND NOT is_disabled))`, change.userID, change.hash, change.checkedHash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 && change.keep == "" {
			return pgx.ErrNoRows
		}
		held := tag.RowsAffected() == 1
		if held && change.keep != "" {
			// The row is held now, so a token that is there stays until the commit.
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM devices WHERE token = $1)", change.keep).Scan(&held); err != nil {
				return err
			}
		}
		if !held {
			return accountRefusal{http.StatusUnauthorized, "unauthorized"}
		}
		return signin.EndTokens(ctx, tx, change.userID, change.keep)
	})
}

// Jellyfin's own providers, which are the only ones there are.
func authenticationProviders(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, []any{map[string]any{"Name": "Default", "Id": defaultAuthenticationProvider}})
}

func passwordResetProviders(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, []any{map[string]any{"Name": "Default Password Reset Provider", "Id": defaultPasswordResetProvider}})
}

const (
	defaultAuthenticationProvider = "Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider"
	defaultPasswordResetProvider  = "Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider"
)
