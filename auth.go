package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Identity without passwords: the first time a browser opens the site, the
// person picks or types their name (John_Doe) once; a long-lived cookie
// remembers it and every order line is stamped with it. Only manager screens
// (purchasing, history, users) ask for a password, once per browser.

const (
	sessionCookie = "mcm_session"
	sessionTTL    = 5 * 365 * 24 * time.Hour
)

var nameRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._'-]*$`)

// normalizeName turns "john doe" / " John_Doe " into "John_Doe".
func normalizeName(raw string) string {
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == '_' || r == '\t' })
	for i, p := range parts {
		r := []rune(p)
		parts[i] = strings.ToUpper(string(r[0])) + string(r[1:])
	}
	return strings.Join(parts, "_")
}

type ctxKey struct{}

type identity struct {
	user  *User
	admin bool // manager mode is on in this browser
	token string
}

func currentUser(r *http.Request) *User { return r.Context().Value(ctxKey{}).(*identity).user }
func currentIdentity(r *http.Request) *identity {
	id, _ := r.Context().Value(ctxKey{}).(*identity)
	return id
}

func (s *Server) identify(r *http.Request) *identity {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	u, admin := s.store.SessionUser(c.Value)
	if u == nil {
		return nil
	}
	return &identity{user: u, admin: admin, token: c.Value}
}

// requireUser wraps handlers that need a known person; admin ones also need
// manager mode.
func (s *Server) requireUser(admin bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := s.identify(r)
		if id == nil {
			writeJSON(w, 401, J{"ok": false, "error": "בחר את שמך כדי להמשיך."})
			return
		}
		if admin && !id.admin {
			writeJSON(w, 403, J{"ok": false, "error": "צריך כניסת מנהל."})
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	}
}

func publicUser(u *User) J {
	return J{"id": u.ID, "name": u.Name, "role": u.Role, "active": u.Active, "has_password": u.PassHash != ""}
}

func hashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

func validPassword(pw string) bool { return len([]rune(pw)) >= 4 }

// GET /api/me: who this browser is, plus what the identify screen needs.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	out := J{"ok": true, "has_admin": s.store.HasAdmin(), "projects": s.store.Projects()}
	if id := s.identify(r); id != nil {
		out["user"], out["admin_mode"] = publicUser(id.user), id.admin
	} else if accessCode() != "" {
		// Behind an office code, strangers don't get the staff list.
		out["user"], out["names"], out["needs_code"] = nil, []string{}, true
	} else {
		names := []string{}
		for _, u := range s.store.Users() {
			if u.Active {
				names = append(names, u.Name)
			}
		}
		out["user"], out["names"] = nil, names
	}
	writeJSON(w, 200, out)
}

// accessCode is the office code (MCM_ACCESS_CODE). When the site is on the
// internet it keeps strangers out: entered once per browser with the name.
func accessCode() string { return strings.TrimSpace(os.Getenv("MCM_ACCESS_CODE")) }

// POST /api/identify {name, code}: "this browser is John_Doe".
func (s *Server) identifyAs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Code string `json:"code"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if want := accessCode(); want != "" &&
		subtle.ConstantTimeCompare([]byte(strings.TrimSpace(in.Code)), []byte(want)) != 1 {
		time.Sleep(500 * time.Millisecond) // slow down guessing
		writeJSON(w, 401, J{"ok": false, "error": "קוד המשרד שגוי."})
		return
	}
	name := normalizeName(in.Name)
	if !nameRe.MatchString(name) || len([]rune(name)) > 60 {
		writeJSON(w, 400, J{"ok": false, "error": "כתוב שם בפורמט John_Doe."})
		return
	}
	u, err := s.store.FindOrCreateUser(name)
	if err != nil {
		writeJSON(w, 500, J{"ok": false, "error": err.Error()})
		return
	}
	if !u.Active {
		writeJSON(w, 403, J{"ok": false, "error": "המשתמש הזה הושבת. פנה למנהל."})
		return
	}
	tok, err := s.store.NewSession(u.ID, sessionTTL)
	if err != nil {
		writeJSON(w, 500, J{"ok": false, "error": err.Error()})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	writeJSON(w, 200, J{"ok": true, "user": publicUser(&u)})
}

// POST /api/forget: "not me" - this browser forgets who it is.
func (s *Server) forget(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.store.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, J{"ok": true})
}

// POST /api/admin/enter {password}: manager mode for this browser. While no
// manager exists yet, the first password entered makes this user the manager.
func (s *Server) adminEnter(w http.ResponseWriter, r *http.Request) {
	id := currentIdentity(r)
	var in struct {
		Password string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if !s.store.HasAdmin() {
		if !validPassword(in.Password) {
			writeJSON(w, 400, J{"ok": false, "error": "סיסמה צריכה להיות לפחות 4 תווים."})
			return
		}
		hash, _ := hashPassword(in.Password)
		if err := s.store.MakeFirstAdmin(id.user.ID, hash); err != nil {
			writeJSON(w, 409, J{"ok": false, "error": "כבר הוגדר מנהל."})
			return
		}
	} else if !id.user.IsAdmin() || bcrypt.CompareHashAndPassword([]byte(id.user.PassHash), []byte(in.Password)) != nil {
		time.Sleep(500 * time.Millisecond) // slow down guessing
		writeJSON(w, 401, J{"ok": false, "error": "סיסמת מנהל שגויה (או שהמשתמש הזה אינו מנהל)."})
		return
	}
	s.store.SetSessionAdmin(id.token, true)
	writeJSON(w, 200, J{"ok": true})
}

func (s *Server) adminLeave(w http.ResponseWriter, r *http.Request) {
	s.store.SetSessionAdmin(currentIdentity(r).token, false)
	writeJSON(w, 200, J{"ok": true})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var in struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if bcrypt.CompareHashAndPassword([]byte(u.PassHash), []byte(in.Old)) != nil {
		writeJSON(w, 400, J{"ok": false, "error": "הסיסמה הנוכחית שגויה."})
		return
	}
	if !validPassword(in.New) {
		writeJSON(w, 400, J{"ok": false, "error": "סיסמה צריכה להיות לפחות 4 תווים."})
		return
	}
	hash, _ := hashPassword(in.New)
	s.store.UpdateUser(u.ID, func(x *User) { x.PassHash = hash })
	writeJSON(w, 200, J{"ok": true})
}

// ---------- manager: users ----------

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	out := []J{}
	for _, u := range s.store.Users() {
		out = append(out, publicUser(&u))
	}
	writeJSON(w, 200, J{"users": out})
}

// PATCH /api/users/{id}: role, active, manager password. Making someone a
// manager needs a password for them (unless they already have one).
func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var in struct {
		Role     *string `json:"role"`
		Active   *bool   `json:"active"`
		Password *string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	var hash string
	if in.Password != nil {
		if !validPassword(*in.Password) {
			writeJSON(w, 400, J{"ok": false, "error": "סיסמה צריכה להיות לפחות 4 תווים."})
			return
		}
		hash, _ = hashPassword(*in.Password)
	}
	errNeedsPassword := errors.New("needs password")
	var inner error
	err := s.store.UpdateUser(id, func(u *User) {
		if in.Role != nil && (*in.Role == "admin" || *in.Role == "user") {
			u.Role = *in.Role
		}
		if in.Active != nil {
			u.Active = *in.Active
		}
		if hash != "" {
			u.PassHash = hash
		}
		if u.IsAdmin() && u.PassHash == "" {
			u.Role, inner = "user", errNeedsPassword
		}
	})
	switch {
	case errors.Is(err, ErrLastAdmin):
		writeJSON(w, 400, J{"ok": false, "error": "חייב להישאר לפחות מנהל פעיל אחד."})
	case err != nil:
		writeJSON(w, 404, J{"ok": false, "error": "משתמש לא נמצא."})
	case inner != nil:
		writeJSON(w, 400, J{"ok": false, "error": "כדי להפוך משתמש למנהל צריך לקבוע לו סיסמת מנהל."})
	default:
		writeJSON(w, 200, J{"ok": true})
	}
}
